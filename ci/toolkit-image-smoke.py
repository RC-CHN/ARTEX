#!/usr/bin/env python3
"""Run offline as UID 10001 with a read-only root and only /tmp writable."""
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.metadata
import json
import os
from pathlib import Path
import shutil
import socket
import ssl
import subprocess
import tempfile
import threading

assert os.getuid() != 0, "must exercise the real non-root Runner path"
Path(os.environ["HOME"]).mkdir(mode=0o700, parents=True, exist_ok=True)


def run(*command, timeout=90, **kwargs):
    p = subprocess.run(command, capture_output=True, text=True, timeout=timeout, **kwargs)
    if p.returncode:
        raise AssertionError((command, p.returncode, p.stdout[-2000:], p.stderr[-2000:]))
    return p.stdout + p.stderr


inventory = json.loads(run("artex-kit", "inventory"))
for name in ("paramiko", "requests", "httpx", "dnspython", "beautifulsoup4", "lxml", "PyYAML", "cryptography", "pycryptodome", "ldap3"):
    assert name in inventory["python"], name
for name in ("ssh", "sshpass", "rsync", "socat", "tcpdump", "strace", "lsof", "busybox", "zip", "xz", "zstd", "subfinder", "dnsx", "naabu", "tlsx", "yq", "chisel"):
    assert shutil.which(name), name

import requests
import httpx
import yaml
from bs4 import BeautifulSoup
from lxml import etree
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from Crypto.Hash import SHA256
import ldap3

assert BeautifulSoup("<p>ready</p>", "html.parser").p.text == "ready"
assert etree.fromstring(b"<root><x>7</x></root>").findtext("x") == "7"
assert yaml.safe_load("value: 7")["value"] == 7
key, nonce = AESGCM.generate_key(bit_length=128), b"0" * 12
cipher = AESGCM(key)
assert cipher.decrypt(nonce, cipher.encrypt(nonce, b"fixture", b""), b"") == b"fixture"
assert SHA256.new(b"fixture").hexdigest()
ldap = ldap3.Connection(ldap3.Server("fixture.invalid"), client_strategy=ldap3.MOCK_SYNC)
assert ldap.bind()
print("Python libraries: real HTTP/parse/crypto/LDAP operations available", flush=True)

catalog = json.loads(Path("/opt/artex-kit/catalog.lock.json").read_text())
for name, tool in catalog["tools"].items():
    if tool.get("native_install", True) is False:
        continue
    assert name in inventory["tools"], name
    flag = "--version" if name in ("yq", "chisel", "jq", "websocat") else "-V" if name == "ffuf" else "-version"
    assert tool["version"] in run(name, flag), name
    # Also verifies shared cache readability and integrity under UID 10001.
    result = json.loads(run("artex-kit", "fetch", name, "--offline"))
    assert result["version"] == tool["version"]
assert "1.10" in run("sqlmap", "--version")
run("testssl", "--version")
run("sshpass", "-V")
print("Pinned tool versions and read-only offline cache: OK", flush=True)

# Every foreign archive is bundled and checksum-pinned; extracting one requires
# only a private writable cache, even with network disabled and root read-only.
import hashlib
portable_root = Path("/opt/artex-kit/portable")
portable = json.loads((portable_root / "catalog.lock.json").read_text())
count = 0
for name, tool in portable["tools"].items():
    assert len(tool["platforms"]) == 20, name
    for platform, entry in tool["platforms"].items():
        with (portable_root / entry["file"]).open("rb") as stream:
            assert hashlib.file_digest(stream, "sha256").hexdigest() == entry["sha256"]
        assert platform in catalog["tools"][name]["platforms"]
        assert catalog["tools"][name]["platforms"][platform].get("file", "").startswith("portable/")
        count += 1
with tempfile.TemporaryDirectory() as temp:
    directory = str(Path(temp) / "mips-tool")
    staged = json.loads(run("artex-kit", "stage", "ffuf", "--platform", "linux/mipsle", "--directory", directory))
    binary = Path(staged["path"]).read_bytes()
    assert binary[:6] == b"\x7fELF\x01\x01" and int.from_bytes(binary[18:20], "little") == 8
    run("artex-kit", "clean", str(Path(directory) / "receipt.json"))
    assert not Path(directory).exists()
    windows = json.loads(run("artex-kit", "fetch", "chisel", "--platform", "windows/arm64", "--offline"))
    assert Path(windows["path"]).read_bytes()[:2] == b"MZ"
print(f"{count} bundled foreign/native archives verified; offline MIPS stage and cleanup: OK", flush=True)


class Target(BaseHTTPRequestHandler):
    seen = set()

    def do_GET(self):
        self.seen.add(self.path)
        if self.path == "/api/data":
            body = b'{"value":7}'
        elif self.path in ("/", "/found", "/linked"):
            body = b'<title>ARTEX fixture</title><a href="/linked">link</a><script>fetch("/api/data")</script>'
        else:
            self.send_response(404)
            self.end_headers()
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/json" if self.path.startswith("/api/") else "text/html")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_):
        pass


target = ThreadingHTTPServer(("127.0.0.1", 0), Target)
thread = threading.Thread(target=target.serve_forever, daemon=True)
thread.start()
base = f"http://127.0.0.1:{target.server_port}"
try:
    assert requests.get(base + "/api/data", timeout=5).json()["value"] == 7
    assert httpx.get(base + "/api/data", timeout=5, trust_env=False).json()["value"] == 7
    with tempfile.TemporaryDirectory() as temp:
        temp = Path(temp)
        words = temp / "words.txt"
        words.write_text("found\nmissing-fixture\n")
        result = temp / "ffuf.json"
        run("ffuf", "-u", base + "/FUZZ", "-w", str(words), "-of", "json", "-o", str(result),
            "-noninteractive", "-s", "-t", "1", "-rate", "5", "-maxtime", "10", "-mc", "200")
        hits = json.loads(result.read_text())["results"]
        assert len(hits) == 1 and hits[0]["url"].endswith("/found"), hits
        result = run("httpx", "-u", base, "-json", "-silent", "-duc", "-timeout", "3", "-retries", "0")
        assert 'ARTEX fixture' in result or base in result, result
        result = run("katana", "-u", base, "-d", "1", "-silent", "-jsonl", "-duc", "-ct", "5s")
        assert "/linked" in result and "/linked" in Target.seen, result
        result = run("yq", "-p=json", "-o=json", ".value + 1", input='{"value":7}')
        assert json.loads(result) == 8
        # Real bundled api-recon script runs without npm ci or a system Chromium.
        script = Path("/api-recon/runtime_harvest.js")
        if script.exists():
            config = temp / "config.json"
            config.write_text(json.dumps({"baseUrl": base, "forward": False, "waitMs": 100,
                                          "perRouteMs": 100, "routes": ["/"], "recordWs": False}))
            run("node", str(script), str(config))
            observed = json.loads((temp / "runtime_api.json").read_text())
            assert observed["provenance"] == "mock-runtime" and not observed["backendVerified"]
            assert any("/api/data" in item for item in observed["uniq"]), observed
    print("Local real-tool discovery and offline api-recon: OK", flush=True)
finally:
    target.shutdown()
    target.server_close()
    thread.join()

# Real TLS negotiation catches missing shell helpers or incompatible OpenSSL
# binaries that version-only checks miss. The certificate belongs to this fixture.
with tempfile.TemporaryDirectory() as temp:
    key, cert = str(Path(temp) / "key.pem"), str(Path(temp) / "cert.pem")
    run("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", key,
        "-out", cert, "-subj", "/CN=localhost", "-days", "1")
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(cert, key)
    tls = ThreadingHTTPServer(("127.0.0.1", 0), Target)
    tls.socket = context.wrap_socket(tls.socket, server_side=True)
    thread = threading.Thread(target=tls.serve_forever, daemon=True)
    thread.start()
    try:
        result = run("testssl", "--quiet", "--color", "0", "--warnings", "off", "--protocols",
                     "--connect-timeout", "3", "--openssl-timeout", "3", f"https://127.0.0.1:{tls.server_port}")
        assert "TLS 1.2" in result and "offered" in result, result
    finally:
        tls.shutdown()
        tls.server_close()
        thread.join()
print("Local TLS negotiation: OK", flush=True)

# A local authoritative DNS fixture exercises both dnspython and the dnsx CLI.
import dns.message
import dns.rrset
sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
sock.bind(("127.0.0.1", 0))
sock.settimeout(0.2)
stop = threading.Event()


def dns_serve():
    while not stop.is_set():
        try:
            data, address = sock.recvfrom(4096)
            query = dns.message.from_wire(data)
            response = dns.message.make_response(query)
            response.answer.append(dns.rrset.from_text(query.question[0].name, 60, "IN", "A", "127.0.0.1"))
            sock.sendto(response.to_wire(), address)
        except (TimeoutError, OSError):
            continue


thread = threading.Thread(target=dns_serve, daemon=True)
thread.start()
try:
    result = run("dnsx", "-r", f"127.0.0.1:{sock.getsockname()[1]}", "-a", "-json", "-silent", "-duc", input="fixture.invalid\n")
    assert "127.0.0.1" in result and "fixture.invalid" in result, result
finally:
    stop.set()
    sock.close()
    thread.join()
print("Local DNS probe: OK", flush=True)

# Same offline/non-root environment; independent HTTP/2 and WebSocket fixtures.
print(run("node", str(Path(__file__).with_name("protocol-tools-smoke.cjs"))), flush=True)

ip_status = json.loads(run("artex-kit", "ipdb", "status"))
assert ip_status["available"], ip_status
ip_result = json.loads(run("artex-kit", "iplookup", "--file", "-",
                          input="8.8.8.8\n2001:4860:4860::8888\n::ffff:8.8.8.8\n10.0.0.1\nexample.invalid\n"))
assert ip_result["offline"] and ip_result["database"]["available"]
for row in ip_result["results"][:3]:
    assert row["asn"]["number"] == 15169 and row["geo"]["status"] == "found", row
assert ip_result["results"][3]["status"] == "special"
assert ip_result["results"][4]["status"] == "invalid"
assert all(s["date"] and s["attribution_url"] for s in ip_result["database"]["sources"].values())
missing = json.loads(run("artex-kit", "iplookup", "--data-dir", "/tmp/missing-ip-data", "8.8.8.8"))
assert not missing["database"]["available"] and missing["results"][0]["status"] == "partial"
print("Offline IPv4/IPv6 geography + ASN, batch input, mapped/private/invalid addresses and missing data: OK", flush=True)
