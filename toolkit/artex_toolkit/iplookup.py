"""Local-only IP enrichment. One immutable snapshot is held for a whole batch."""
from contextlib import ExitStack
from datetime import date, datetime, timezone
import ipaddress
import json
import os
from pathlib import Path

DEFAULT_DIRECTORY = "/opt/artex-kit/ip-data"
MAX_INPUTS = 10000
FILENAMES = {"ipv4": "ipv4.xdb", "ipv6": "ipv6.xdb", "asn": "asn.mmdb"}


def directory(value=None):
    return Path(value or os.environ.get("ARTEX_IP_DATA", DEFAULT_DIRECTORY))


def snapshot(root):
    root = Path(root).resolve()
    current = (root / "current").resolve(strict=True)
    if not current.is_relative_to(root / "sets"):
        raise ValueError("IP database snapshot escaped its directory")
    path = current / "manifest.json"
    if path.is_symlink() or path.stat().st_size > 65536:
        raise ValueError("IP database manifest is too large")
    manifest = json.loads(path.read_text())
    if manifest.get("schema") != 1 or set(manifest.get("sources", {})) != set(FILENAMES):
        raise ValueError("invalid IP database manifest")
    for name, entry in manifest["sources"].items():
        if entry.get("filename") != FILENAMES[name]:
            raise ValueError("invalid IP database filename")
    return current, manifest


def describe(root):
    try:
        current, manifest = snapshot(root)
        sources = {}
        today = datetime.now(timezone.utc).date()
        for name, entry in manifest["sources"].items():
            age = (today - date.fromisoformat(entry["date"])).days
            path = current / entry["filename"]
            sources[name] = {key: entry[key] for key in ("provider", "date", "license", "attribution_url", "data_sha256")}
            sources[name].update(age_days=age, stale=age > 90,
                                 available=path.is_file() and not path.is_symlink() and path.stat().st_size == entry["data_size"])
        return {"available": all(s["available"] for s in sources.values()),
                "snapshot": current.name, "sources": sources}
    except (OSError, ValueError, KeyError, TypeError):
        return {"available": False, "error": "missing_or_invalid_database"}


def open_reader(name, path):
    if name == "asn":
        import maxminddb
        reader = maxminddb.open_database(str(path))
        if "ASN" not in reader.metadata().database_type:
            reader.close()
            raise ValueError("expected an ASN MMDB")
        return reader
    from ip2region import util, searcher
    version = util.IPv4 if name == "ipv4" else util.IPv6
    with path.open("rb") as stream:
        util.verify(stream)
        header = util.load_header(stream)
        if util.version_from_header(header) != version:
            raise ValueError("XDB address family mismatch")
        size = os.fstat(stream.fileno()).st_size
        if not (256 + util.VectorIndexLength <= header.startIndexPtr <= header.endIndexPtr < size):
            raise ValueError("invalid XDB index bounds")
        if header.endIndexPtr + version.index_size > size:
            raise ValueError("truncated XDB index")
    # File-only mode reads a small index path per query, never the full database.
    return searcher.new_with_file_only(version, str(path))


def classify(value):
    if not isinstance(value, str) or len(value) > 128 or "%" in value:
        raise ValueError("expected an IP literal without a scope identifier")
    ip = ipaddress.ip_address(value.strip())
    if isinstance(ip, ipaddress.IPv6Address) and ip.ipv4_mapped:
        ip = ip.ipv4_mapped
    for attr, label in (("is_unspecified", "unspecified"), ("is_loopback", "loopback"),
                        ("is_link_local", "link_local"), ("is_multicast", "multicast")):
        if getattr(ip, attr):
            return ip, label
    private = ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16") if ip.version == 4 else ("fc00::/7",)
    if any(ip in ipaddress.ip_network(network) for network in private):
        return ip, "private"
    if ip.version == 4 and ip in ipaddress.ip_network("100.64.0.0/10"):
        return ip, "shared_address_space"
    if not ip.is_global or ip.is_reserved:
        return ip, "reserved"
    return ip, "public"


class IPDatabase:
    """Single-threaded reader; each CLI batch owns its own XDB file handles."""
    def __init__(self, root):
        self.stack = ExitStack()
        self.readers, self.errors = {}, {}
        self.metadata = {"available": False, "error": "missing_or_invalid_database"}
        try:
            current, manifest = snapshot(root)
            # Resolve once: concurrent updates cannot mix source generations.
            today = datetime.now(timezone.utc).date()
            sources = {}
            for name, entry in manifest["sources"].items():
                age = (today - date.fromisoformat(entry["date"])).days
                sources[name] = {key: entry[key] for key in ("provider", "date", "license", "attribution_url", "data_sha256")}
                sources[name].update(age_days=age, stale=age > 90)
                try:
                    path = current / entry["filename"]
                    if path.is_symlink() or path.stat().st_size != entry["data_size"]:
                        raise ValueError("database size mismatch")
                    reader = open_reader(name, path)
                    self.stack.callback(reader.close)
                    self.readers[name] = reader
                except (OSError, ValueError, RuntimeError, ImportError, IndexError):
                    self.errors[name] = "database_unavailable"
            self.metadata = {"available": not self.errors, "snapshot": current.name, "sources": sources,
                             "errors": self.errors}
        except (OSError, ValueError, KeyError, TypeError):
            self.stack.close()
            self.readers.clear()

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.stack.close()

    def lookup(self, value):
        result = {"input": value}
        try:
            ip, kind = classify(value)
        except ValueError:
            return {**result, "status": "invalid", "error": "expected_ip_literal"}
        result.update(ip=str(ip), family=ip.version, address_type=kind)
        if kind != "public":
            return {**result, "status": "special", "geo": None, "asn": None}
        for field, name in (("geo", "ipv4" if ip.version == 4 else "ipv6"), ("asn", "asn")):
            item = {"source": name, "status": "unavailable"}
            reader = self.readers.get(name)
            if reader is not None:
                try:
                    if field == "geo":
                        raw = reader.search(str(ip))
                        if raw:
                            values = raw.split("|")
                            if len(values) != 5:
                                raise ValueError("unexpected ip2region fields")
                            item.update(zip(("country", "region", "city", "isp", "country_code"),
                                            (None if v in ("", "0") else v for v in values)))
                        item["status"] = "found" if raw else "not_found"
                    else:
                        raw, prefix = reader.get_with_prefix_len(str(ip))
                        if raw:
                            number = raw.get("autonomous_system_number")
                            organization = raw.get("autonomous_system_organization")
                            if not isinstance(number, int) or not 0 < number <= 4294967295 or not isinstance(organization, str):
                                raise ValueError("unexpected ASN fields")
                            item.update(number=number, organization=organization,
                                        network=str(ipaddress.ip_network(f"{ip}/{prefix}", strict=False)))
                        item["status"] = "found" if raw else "not_found"
                except (OSError, ValueError, RuntimeError, IndexError, KeyError, TypeError):
                    item = {"source": name, "status": "error", "error": "database_read_failed"}
            result[field] = item
        states = {result[field]["status"] for field in ("geo", "asn")}
        result["status"] = "partial" if states & {"error", "unavailable"} else "found" if "found" in states else "not_found"
        return result


def inputs(addresses, stream=None):
    values = list(addresses)
    if len(values) > MAX_INPUTS:
        raise ValueError("IP batch exceeds 10000 addresses")
    if stream is not None:
        while line := stream.readline(258):
            if len(line) > 256:
                raise ValueError("IP input line exceeds 256 characters")
            if value := line.strip():
                values.append(value)
                if len(values) > MAX_INPUTS:
                    raise ValueError("IP batch exceeds 10000 addresses")
    if not values:
        raise ValueError("provide IP literals or --file (one IP per line)")
    return values


def lookup_many(root, values):
    with IPDatabase(root) as db:
        return {"offline": True, "database": db.metadata, "results": [db.lookup(value) for value in values]}
