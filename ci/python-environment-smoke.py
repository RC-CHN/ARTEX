#!/usr/bin/env python3
"""Exercise the actual locked Python environment offline, without privileges."""
import asyncio
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
import importlib
import importlib.metadata
import io
import os
from pathlib import Path
import re
import select
import socket
import socketserver
import struct
import subprocess
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from artex_toolkit.environment import PYTHON_MODULES
from packaging.utils import canonicalize_name

assert os.getuid() != 0, "test the real non-root Runner environment"
Path(os.environ["HOME"]).mkdir(parents=True, exist_ok=True)
os.chdir(os.environ["HOME"])
installed = list(importlib.metadata.distributions())
duplicates = Counter(canonicalize_name(d.metadata["Name"]) for d in installed)
assert all(count == 1 for count in duplicates.values()), duplicates
lock = Path("/opt/artex-tools/requirements.lock").read_text()
for name, version in re.findall(r"^([A-Za-z0-9_-]+)==([^\s\\]+)", lock, re.M):
    assert importlib.metadata.version(name) == version, name
for distribution, module in PYTHON_MODULES.items():
    importlib.metadata.version(distribution)
    importlib.import_module(module)
subprocess.run(["python3", "-m", "pip", "check"], check=True)
print(f"All {len(PYTHON_MODULES)} advertised distributions import successfully", flush=True)


class HTTPFixture(BaseHTTPRequestHandler):
    def do_GET(self):
        body = b"offline-proxy-fixture"
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


def receive(sock, length):
    result = b""
    while len(result) < length:
        part = sock.recv(length - len(result))
        if not part:
            raise EOFError("short SOCKS handshake")
        result += part
    return result


class SOCKSFixture(socketserver.BaseRequestHandler):
    domains = []

    def handle(self):
        self.request.settimeout(10)
        version, methods = receive(self.request, 2)
        assert version == 5 and 0 in receive(self.request, methods)
        self.request.sendall(b"\x05\x00")
        version, command, reserved, address_type = receive(self.request, 4)
        assert (version, command, reserved, address_type) == (5, 1, 0, 3)
        domain = receive(self.request, receive(self.request, 1)[0]).decode()
        port = struct.unpack("!H", receive(self.request, 2))[0]
        assert domain == "proxy-fixture.invalid" and port == http.server_port
        self.domains.append(domain)
        # Only this local fixture is reachable; never resolve or relay an external host.
        with socket.create_connection(("127.0.0.1", port), timeout=10) as upstream:
            self.request.sendall(b"\x05\x00\x00\x01\x7f\x00\x00\x01\x00\x00")
            while True:
                ready, _, _ = select.select([self.request, upstream], [], [], 10)
                if not ready:
                    return
                for source in ready:
                    data = source.recv(65536)
                    if not data:
                        return
                    (upstream if source is self.request else self.request).sendall(data)


http = ThreadingHTTPServer(("127.0.0.1", 0), HTTPFixture)
proxy = socketserver.ThreadingTCPServer(("127.0.0.1", 0), SOCKSFixture)
proxy.daemon_threads = True
servers = [http, proxy]
threads = [threading.Thread(target=s.serve_forever, daemon=True) for s in servers]
for thread in threads:
    thread.start()
try:
    import requests
    import httpx
    import aiohttp
    from aiohttp_socks import ProxyConnector

    proxy_url = f"socks5h://127.0.0.1:{proxy.server_address[1]}"
    url = f"http://proxy-fixture.invalid:{http.server_port}/"
    with requests.Session() as client:
        client.trust_env = False
        assert client.get(url, proxies={"http": proxy_url}, timeout=10).text == "offline-proxy-fixture"
    with httpx.Client(proxy=proxy_url, http2=True, timeout=10, trust_env=False) as client:
        assert client.get(url).text == "offline-proxy-fixture"

    async def async_proxy():
        connector = ProxyConnector.from_url(proxy_url.replace("socks5h:", "socks5:"), rdns=True)
        async with aiohttp.ClientSession(connector=connector, timeout=aiohttp.ClientTimeout(total=10)) as client:
            async with client.get(url) as response:
                assert await response.text() == "offline-proxy-fixture"
    asyncio.run(async_proxy())
    assert len(SOCKSFixture.domains) == 3
finally:
    for server in servers:
        server.shutdown()
        server.server_close()
    for thread in threads:
        thread.join(timeout=5)
print("requests, HTTPX and aiohttp: real SOCKS5 connections with remote DNS", flush=True)

# WebSocket: both advertised client APIs talk to a bounded local echo server.
from websockets.sync.server import serve
from websockets.sync.client import connect
import websocket


def echo(ws):
    for message in ws:
        ws.send(message)


with serve(echo, "127.0.0.1", 0) as server:
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    address = f"ws://127.0.0.1:{server.socket.getsockname()[1]}"
    try:
        with connect(address, proxy=None, open_timeout=5) as client:
            client.send("fixture")
            assert client.recv(timeout=5) == "fixture"
        client = websocket.create_connection(address, timeout=5, http_no_proxy=["127.0.0.1"])
        try:
            client.send("fixture")
            assert client.recv() == "fixture"
        finally:
            client.close()
    finally:
        server.shutdown()
        thread.join(timeout=5)

# gRPC: descriptor compilation, actual RPC, and server reflection.
import grpc
from grpc_reflection.v1alpha import reflection, reflection_pb2, reflection_pb2_grpc
from grpc_tools import protoc

with tempfile.TemporaryDirectory() as directory:
    Path(directory, "fixture.proto").write_text('syntax = "proto3"; message Fixture { string value = 1; }')
    assert protoc.main(["protoc", "-I" + directory, "--descriptor_set_out=" + directory + "/fixture.pb",
                        directory + "/fixture.proto"]) == 0
    assert Path(directory, "fixture.pb").stat().st_size > 0
with ThreadPoolExecutor(max_workers=2) as pool:
    server = grpc.server(pool)
    server.add_generic_rpc_handlers((grpc.method_handlers_generic_handler("fixture.Echo", {
        "Say": grpc.unary_unary_rpc_method_handler(lambda message, ctx: message),
    }),))
    reflection.enable_server_reflection(("fixture.Echo", reflection.SERVICE_NAME), server)
    port = server.add_insecure_port("127.0.0.1:0")
    server.start()
    try:
        with grpc.insecure_channel(f"127.0.0.1:{port}") as channel:
            assert channel.unary_unary("/fixture.Echo/Say")(b"fixture", timeout=5) == b"fixture"
            stub = reflection_pb2_grpc.ServerReflectionStub(channel)
            responses = list(stub.ServerReflectionInfo(iter([
                reflection_pb2.ServerReflectionRequest(list_services="")]), timeout=5))
            assert "fixture.Echo" in [s.name for s in responses[0].list_services_response.service]
    finally:
        server.stop(0).wait(timeout=5)
print("WebSocket clients, protobuf compiler, gRPC calls and reflection: OK", flush=True)

# Offline parsing, serialization, JWT and device protocols do not need network access.
import jwt
import tldextract
import cbor2
import msgpack
import construct
import serial
import dpkt
from scapy.all import IP, UDP, Raw
from defusedxml import ElementTree
from defusedxml.common import DefusedXmlException
from paho.mqtt.client import Client, CallbackAPIVersion
from pymodbus.client import ModbusTcpClient

extract = tldextract.TLDExtract(suffix_list_urls=(), cache_dir=None)
assert extract("https://service.example.co.jp/path").top_domain_under_public_suffix == "example.co.jp"
value = {"fixture": [1, 2, 3]}
assert cbor2.loads(cbor2.dumps(value)) == value
assert msgpack.unpackb(msgpack.packb(value)) == value
record = construct.Struct("value" / construct.Int32ul)
assert record.parse(record.build({"value": 42})).value == 42
key = "local-test-key-32-bytes-long-only!"
token = jwt.encode({"sub": "fixture"}, key, algorithm="HS256")
assert jwt.decode(token, key, algorithms=["HS256"])["sub"] == "fixture"
try:
    ElementTree.fromstring('<!DOCTYPE a [<!ENTITY e "fixture">]><a>&e;</a>')
    raise AssertionError("XML entity was accepted")
except DefusedXmlException:
    pass
with serial.serial_for_url("loop://", timeout=1) as port:
    port.write(b"fixture")
    assert port.read(7) == b"fixture"
packet = bytes(IP(dst="127.0.0.1") / UDP(dport=12345) / Raw(b"fixture"))
assert dpkt.ip.IP(packet).data.data == b"fixture"
pcap = io.BytesIO()
dpkt.pcap.Writer(pcap, linktype=dpkt.pcap.DLT_RAW).writepkt(packet, ts=1)
pcap.seek(0)
assert list(dpkt.pcap.Reader(pcap))[0][1] == packet
Client(CallbackAPIVersion.VERSION2)
ModbusTcpClient("127.0.0.1").close()
print("Offline suffix snapshot, codecs, JWT, XML, serial loopback and PCAP: OK", flush=True)

# Exercise native binary-analysis engines rather than merely importing their wrappers.
import capstone
import unicorn
from unicorn.x86_const import UC_X86_REG_EAX
import angr
import claripy
from pwnlib.util.packing import p32, u32
from pwnlib.asm import asm
from pwnlib.context import context
from elftools.elf.elffile import ELFFile

code = bytes.fromhex("b82a000000")  # mov eax, 42, a harmless arithmetic fixture
assert next(capstone.Cs(capstone.CS_ARCH_X86, capstone.CS_MODE_32).disasm(code, 0)).mnemonic == "mov"
emulator = unicorn.Uc(unicorn.UC_ARCH_X86, unicorn.UC_MODE_32)
emulator.mem_map(0x1000, 0x1000)
emulator.mem_write(0x1000, code)
emulator.emu_start(0x1000, 0x1000 + len(code), count=1)
assert emulator.reg_read(UC_X86_REG_EAX) == 42
project = angr.load_shellcode(code, arch="x86", load_address=0x1000)
state = project.factory.blank_state(addr=0x1000)
successors = project.factory.successors(state, num_inst=1)
assert successors.successors[0].solver.eval(successors.successors[0].regs.eax) == 42
symbol = claripy.BVS("fixture", 8)
solver = claripy.Solver()
solver.add(symbol + 1 == 42)
assert tuple(solver.eval(symbol, 1)) == (41,)
assert u32(p32(42)) == 42
with context.local(arch="amd64" if os.uname().machine == "x86_64" else "aarch64"):
    assert asm("nop")
with open("/usr/local/bin/python3", "rb") as binary:
    assert ELFFile(binary).elfclass == 64
print("Capstone, Unicorn, angr/VEX, Z3, pwntools and native binutils: OK", flush=True)
print("PYTHON ENVIRONMENT SMOKE PASSED", flush=True)
