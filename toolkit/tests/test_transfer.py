"""Real Paramiko handshake and SFTP round trips against an isolated temp root."""
import hashlib
import os
from pathlib import Path
import socket
import stat
import tempfile
import threading
import unittest

try:
    import paramiko
except ImportError:
    paramiko = None

from artex_toolkit.transfer import connect, deploy, cleanup


@unittest.skipIf(paramiko is None, "install locked Runner Python requirements for SSH integration")
class TransferTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.remote = self.root / "target"
        self.remote.mkdir()
        root = self.remote
        self.key = paramiko.RSAKey.generate(2048)
        self.client_key = paramiko.RSAKey.generate(2048)
        public = self.client_key.asbytes()
        self.key_path = self.root / "identity"
        self.client_key.write_private_key_file(str(self.key_path))
        os.chmod(self.key_path, 0o600)

        class Auth(paramiko.ServerInterface):
            def check_auth_publickey(self, username, key):
                return paramiko.AUTH_SUCCESSFUL if username == "fixture" and key.asbytes() == public else paramiko.AUTH_FAILED

            def get_allowed_auths(self, _):
                return "publickey"

            def check_channel_request(self, kind, chanid):
                return paramiko.OPEN_SUCCEEDED if kind == "session" else paramiko.OPEN_FAILED_ADMINISTRATIVELY_PROHIBITED

        class Files(paramiko.SFTPServerInterface):
            def path(self, name):
                p = (root / name.lstrip("/")).resolve()
                if not p.is_relative_to(root):
                    raise OSError("outside fixture root")
                return p

            def stat(self, path):
                try:
                    return paramiko.SFTPAttributes.from_stat(self.path(path).stat())
                except OSError as e:
                    return paramiko.SFTPServer.convert_errno(e.errno)

            lstat = stat

            def list_folder(self, path):
                result = []
                for p in self.path(path).iterdir():
                    item = paramiko.SFTPAttributes.from_stat(p.stat())
                    item.filename = p.name
                    result.append(item)
                return result

            def mkdir(self, path, attr):
                self.path(path).mkdir(mode=attr.st_mode or 0o700)
                return paramiko.SFTP_OK

            def rmdir(self, path):
                self.path(path).rmdir()
                return paramiko.SFTP_OK

            def remove(self, path):
                self.path(path).unlink()
                return paramiko.SFTP_OK

            def rename(self, old, new):
                self.path(old).rename(self.path(new))
                return paramiko.SFTP_OK

            def chattr(self, path, attr):
                if attr.st_mode is not None:
                    self.path(path).chmod(attr.st_mode)
                return paramiko.SFTP_OK

            def open(self, path, flags, attr):
                try:
                    fd = os.open(self.path(path), flags, 0o600)
                    mode = "wb" if flags & os.O_WRONLY else "r+b" if flags & os.O_RDWR else "rb"
                    f = os.fdopen(fd, mode)
                    handle = paramiko.SFTPHandle(flags)
                    handle.readfile = f
                    handle.writefile = f
                    return handle
                except OSError as e:
                    return paramiko.SFTPServer.convert_errno(e.errno)

        self.socket = socket.socket()
        self.socket.bind(("127.0.0.1", 0))
        self.socket.listen()
        self.socket.settimeout(0.2)
        self.port = self.socket.getsockname()[1]
        self.stop = threading.Event()
        self.transports = []

        def serve():
            while not self.stop.is_set():
                try:
                    sock, _ = self.socket.accept()
                except (TimeoutError, OSError):
                    continue
                transport = paramiko.Transport(sock)
                self.transports.append(transport)
                transport.add_server_key(self.key)
                transport.set_subsystem_handler("sftp", paramiko.SFTPServer, Files)
                try:
                    transport.start_server(server=Auth())
                except paramiko.SSHException:
                    transport.close()
        self.thread = threading.Thread(target=serve, daemon=True)
        self.thread.start()
        self.addCleanup(self.shutdown)
        self.known = self.root / "known_hosts"
        self.known.write_text(f"[127.0.0.1]:{self.port} {self.key.get_name()} {self.key.get_base64()}\n")

    def shutdown(self):
        self.stop.set()
        self.socket.close()
        for transport in self.transports:
            transport.close()
        self.thread.join(timeout=5)

    def test_real_host_verification_stage_integrity_and_conservative_cleanup(self):
        with self.assertRaises(paramiko.SSHException):
            connect("127.0.0.1", "fixture", self.port, str(self.key_path))
        client = connect("127.0.0.1", "fixture", self.port, str(self.key_path), str(self.known))
        self.addCleanup(client.close)
        source = self.root / "tool"
        source.write_bytes(b"synthetic-tool\x00" * 1000)
        artifact = {"path": str(source), "filename": "tool", "binary_sha256": hashlib.sha256(source.read_bytes()).hexdigest()}
        license_file = source.parent / "LICENSE"
        license_file.write_text("Synthetic fixture license")
        artifact["files"] = {"tool": artifact["binary_sha256"], "LICENSE": hashlib.sha256(license_file.read_bytes()).hexdigest()}
        pending = []
        with client.open_sftp() as sftp:
            receipt = deploy(sftp, artifact, "/", {"host": "fixture"}, on_pending=lambda r: pending.append(dict(r)))
            self.assertEqual(pending[0]["state"], "staging")
            self.assertEqual(receipt["state"], "ready")
            remote = self.remote / receipt["remote_path"].lstrip("/")
            self.assertEqual(remote.read_bytes(), source.read_bytes())
            self.assertEqual((remote.parent / "LICENSE").read_bytes(), license_file.read_bytes())
            extra = remote.parent / "operator-file"
            extra.write_text("preserve")
            with self.assertRaisesRegex(ValueError, "unowned"):
                cleanup(sftp, receipt)
            extra.unlink()
            remote.write_text("modified")
            with self.assertRaisesRegex(ValueError, "changed"):
                cleanup(sftp, receipt)
            remote.write_bytes(source.read_bytes())
            self.assertEqual(cleanup(sftp, receipt)["state"], "cleaned")
            self.assertEqual(list(self.remote.iterdir()), [])

    def test_transfer_failure_keeps_pending_receipt_and_removes_partial_file(self):
        client = connect("127.0.0.1", "fixture", self.port, str(self.key_path), str(self.known))
        self.addCleanup(client.close)
        source = self.root / "tool"
        source.write_bytes(b"wrong digest")
        pending = []
        with client.open_sftp() as sftp:
            with self.assertRaisesRegex(ValueError, "checksum"):
                deploy(sftp, {"path": str(source), "filename": "tool", "binary_sha256": "0" * 64}, "/", {},
                       on_pending=lambda r: pending.append(dict(r)))
        self.assertEqual(len(pending), 1)
        self.assertEqual(list(self.remote.iterdir()), [])
