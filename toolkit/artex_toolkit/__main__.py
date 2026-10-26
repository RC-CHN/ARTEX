import argparse
import importlib.metadata
import json
import os
from pathlib import Path
import shutil
import sys
import urllib.parse
import urllib.request

from . import __version__
from .catalog import Catalog, atomic_json, clean_stage, host_platform
from .environment import PYTHON_MODULES


def rpc(endpoint, payload=None):
    base = os.environ.get("ARTEX_KIT_URL", "http://127.0.0.1:8790").rstrip("/")
    url = urllib.parse.urlsplit(base)
    if url.scheme != "https" and not (url.scheme == "http" and url.hostname in ("127.0.0.1", "::1", "localhost")):
        raise ValueError("remote coordinator connections require HTTPS")
    if url.username or url.password or url.query or url.fragment:
        raise ValueError("invalid coordinator URL")
    token = Path(os.environ["ARTEX_KIT_TOKEN_FILE"]).read_text().strip()
    req = urllib.request.Request(base + endpoint, data=None if payload is None else json.dumps(payload).encode(),
                                 headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
    # Do not forward bearer credentials to a redirect target or ambient proxy.
    from .coordinator import NoRedirect
    with urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect()).open(req, timeout=180) as res:
        return json.load(res)


def main(argv=None):
    p = argparse.ArgumentParser(prog="artex-kit", description="Pinned tools, portable staging and reproducible verification")
    p.add_argument("--version", action="version", version=__version__)
    p.add_argument("--catalog", default=os.environ.get("ARTEX_TOOL_CATALOG", str(Path(__file__).parent.parent / "catalog.lock.json")))
    p.add_argument("--cache", default=os.environ.get("ARTEX_TOOL_CACHE", str(Path.home() / ".cache/artex-kit")))
    sub = p.add_subparsers(dest="command", required=True)
    sub.add_parser("inventory")
    sub.add_parser("catalog")
    iplookup = sub.add_parser("iplookup", help="Offline IPv4/IPv6 geography and ASN lookup; no DNS or API requests")
    iplookup.add_argument("addresses", nargs="*")
    iplookup.add_argument("--file", help="One IP per line, or - for stdin; max 10000")
    iplookup.add_argument("--data-dir", help="Read-only IP data directory (or ARTEX_IP_DATA)")
    ipdb = sub.add_parser("ipdb", help="Explicit IP database installation and version reporting")
    ipdb.add_argument("action", choices=("status", "install"))
    ipdb.add_argument("--data-dir")
    ipdb.add_argument("--manifest", default=str(Path(__file__).parent.parent / "ip-data.lock.json"))
    ipdb.add_argument("--archives", default=os.environ.get("ARTEX_TOOL_ARCHIVES"))
    ipdb.add_argument("--offline", action="store_true")
    fetch = sub.add_parser("fetch")
    stage = sub.add_parser("stage")
    transfer = sub.add_parser("transfer")
    for cmd in (fetch, stage, transfer):
        cmd.add_argument("tool")
        cmd.add_argument("--platform", default=host_platform())
    fetch.add_argument("--offline", action="store_true")
    fetch.add_argument("--archive", help="Import an already-downloaded archive after checksum verification")
    stage.add_argument("--directory", required=True, help="New directory owned by this operation")
    clean = sub.add_parser("clean")
    clean.add_argument("receipt")
    remote_clean = sub.add_parser("remote-clean")
    remote_clean.add_argument("receipt")
    for cmd in (transfer, remote_clean):
        cmd.add_argument("--host", required=True)
        cmd.add_argument("--user", required=True)
        cmd.add_argument("--port", type=int, default=22)
        cmd.add_argument("--key")
        cmd.add_argument("--known-hosts")
    transfer.add_argument("--base", required=True, help="Existing absolute SFTP directory")
    transfer.add_argument("--receipt", required=True, help="Private local transfer receipt")
    serve = sub.add_parser("serve")
    serve.add_argument("--config", required=True)
    serve.add_argument("--state", required=True)
    serve.add_argument("--listen", default="127.0.0.1")
    serve.add_argument("--port", type=int, default=8790)
    run = sub.add_parser("verify")
    run.add_argument("plan")
    run.add_argument("--output", required=True)
    status = sub.add_parser("status")
    status.add_argument("id")
    callback = sub.add_parser("callback")
    callback.add_argument("run_id")
    callback.add_argument("--ttl", type=int, default=3600)
    args = p.parse_args(argv)
    if args.command in ("iplookup", "ipdb"):
        from .iplookup import describe, directory, inputs, lookup_many
        root = directory(args.data_dir)
        if args.command == "ipdb":
            if args.action == "status":
                return describe(root)
            from .ipdata import install
            return install(args.manifest, root, args.archives, args.offline)
        if args.file == "-":
            values = inputs(args.addresses, sys.stdin)
        elif args.file:
            with open(args.file) as stream:
                values = inputs(args.addresses, stream)
        else:
            values = inputs(args.addresses)
        return lookup_many(root, values)
    if args.command == "inventory":
        from .iplookup import describe, directory
        packages = {}
        for name in PYTHON_MODULES:
            try:
                packages[name] = importlib.metadata.version(name)
            except importlib.metadata.PackageNotFoundError:
                pass
        return {"version": __version__, "platform": host_platform(), "python": packages,
                "python_imports": {name: PYTHON_MODULES[name] for name in packages},
                "tools": {name: shutil.which(name) for name in (
                    "httpx", "katana", "ffuf", "sqlmap", "testssl", "nuclei", "vulnx", "interactsh-client", "jq", "yq", "chisel",
                    "subfinder", "dnsx", "naabu", "tlsx", "grpcurl", "websocat", "tack", "tack-control", "tack-node", "ssh", "sshpass", "rsync", "socat", "tcpdump", "busybox",
                    "python3", "node", "npm", "playwright-cli", "pwn", "readelf", "objdump", "as", "ld", "cpp") if shutil.which(name)},
                "ip_database": describe(directory()),
                "coordinator_configured": bool(os.environ.get("ARTEX_KIT_TOKEN_FILE"))}
    if args.command == "serve":
        from .coordinator import Coordinator, make_server
        cfg = Path(args.config)
        if cfg.stat().st_mode & 0o077:
            raise ValueError("coordinator config contains secrets; require mode 0600")
        coordinator = Coordinator(json.loads(cfg.read_text()), args.state)
        server = make_server(coordinator, (args.listen, args.port))
        try:
            server.serve_forever()
        except KeyboardInterrupt:
            pass
        finally:
            server.server_close()
            coordinator.close()
        return {"stopped": True}
    if args.command == "verify":
        result = rpc("/runs", json.loads(Path(args.plan).read_text()))
        atomic_json(args.output, result)
        return result
    if args.command == "status":
        return rpc("/runs/" + args.id)
    if args.command == "callback":
        return rpc("/callbacks", {"run_id": args.run_id, "ttl": args.ttl})
    if args.command == "clean":
        return clean_stage(args.receipt)
    catalog = Catalog(args.catalog, args.cache)
    if args.command == "catalog":
        return catalog.data
    if args.command == "fetch":
        return catalog.fetch(args.tool, args.platform, args.offline, args.archive)
    if args.command == "stage":
        return catalog.stage(args.tool, args.platform, args.directory)
    if args.command in ("transfer", "remote-clean"):
        from . import transfer as ssh
        context = {"host": args.host, "user": args.user, "port": args.port}
        if args.command == "remote-clean":
            receipt = json.loads(Path(args.receipt).read_text())
            if any(receipt[k] != v for k, v in context.items()):
                raise ValueError("receipt belongs to a different SSH endpoint")
        else:
            artifact = catalog.fetch(args.tool, args.platform, offline=True)
            # Save endpoint and intent before network effects; failed transfers
            # remain discoverable rather than looking like successful execution.
            atomic_json(args.receipt, {**context, "state": "connecting", "tool": args.tool, "platform": args.platform})
        client = ssh.connect(args.host, args.user, args.port, args.key, args.known_hosts)
        try:
            with client.open_sftp() as sftp:
                if args.command == "remote-clean":
                    result = ssh.cleanup(sftp, receipt)
                    atomic_json(args.receipt, {**receipt, **result})
                else:
                    result = ssh.deploy(sftp, artifact, args.base, context,
                                        on_pending=lambda r: atomic_json(args.receipt, r))
                    atomic_json(args.receipt, result)
                return result
        finally:
            client.close()
    raise ValueError("unsupported command")


def entrypoint():
    try:
        print(json.dumps(main(), ensure_ascii=False, indent=2))
    except Exception as e:
        # Network/library errors may contain secrets; only our validation errors
        # are safe to explain verbatim. The original artifact is kept on disk.
        print(json.dumps({"error": str(e) if isinstance(e, ValueError) else type(e).__name__}), file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    entrypoint()
