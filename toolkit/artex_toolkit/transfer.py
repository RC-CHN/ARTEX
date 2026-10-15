"""SFTP staging over an existing SSH identity; never runs the uploaded tool."""
import hashlib
import json
import posixpath
import re
import secrets
import stat

from pathlib import Path
from .catalog import NAME


def connect(host, username, port=22, key=None, known_hosts=None):
    import paramiko
    client = paramiko.SSHClient()
    client.load_system_host_keys()
    if known_hosts:
        client.load_host_keys(known_hosts)
    client.set_missing_host_key_policy(paramiko.RejectPolicy())
    client.connect(host, port=port, username=username, key_filename=key,
                   timeout=15, auth_timeout=15, banner_timeout=15)
    return client


def remote_digest(sftp, path):
    with sftp.open(path, "rb") as f:
        h = hashlib.sha256()
        while chunk := f.read(1024 * 1024):
            h.update(chunk)
        return h.hexdigest()


def deploy(sftp, artifact, base, context, on_pending=lambda receipt: None):
    if not base.startswith("/") or ".." in base.split("/") or "\x00" in base:
        raise ValueError("remote base must be an absolute SFTP directory")
    if not stat.S_ISDIR(sftp.lstat(base).st_mode):
        raise ValueError("remote base is not a regular directory")
    directory = posixpath.join(base, "artex-" + secrets.token_hex(12))
    dest = posixpath.join(directory, artifact["filename"])
    receipt = {**artifact, **context, "remote_directory": directory, "remote_path": dest,
               "state": "staging"}
    # Caller persists a pending receipt before transfer, so interrupted uploads
    # remain visible and are never reported as installed.
    files = artifact.get("files", {artifact["filename"]: artifact["binary_sha256"]})
    if any(not NAME.fullmatch(name) for name in files):
        raise ValueError("invalid transfer filename")
    on_pending(receipt)
    sftp.mkdir(directory, mode=0o700)
    created = []
    try:
        for filename, expected in files.items():
            target = posixpath.join(directory, filename)
            created.append(target + ".part")
            sftp.put(str(Path(artifact["path"]).parent / filename), target + ".part", confirm=True)
            if remote_digest(sftp, target + ".part") != expected:
                raise ValueError("remote checksum mismatch")
            sftp.chmod(target + ".part", 0o700 if filename == artifact["filename"] else 0o600)
            sftp.rename(target + ".part", target)
            created[-1] = target
        receipt["state"] = "ready"
        return receipt
    except Exception:
        # Only touch paths created by this operation, never a recursive deletion.
        for target in created:
            try:
                sftp.remove(target)
            except OSError:
                pass
        try:
            sftp.rmdir(directory)
        except OSError:
            pass
        raise


def cleanup(sftp, receipt):
    directory = receipt["remote_directory"]
    name = posixpath.basename(directory)
    if not re.fullmatch(r"artex-[a-f0-9]{24}", name):
        raise ValueError("invalid transfer directory")
    dest = posixpath.join(directory, receipt["filename"])
    if dest != receipt["remote_path"] or posixpath.basename(receipt["filename"]) != receipt["filename"]:
        raise ValueError("invalid transfer receipt")
    if not stat.S_ISDIR(sftp.lstat(directory).st_mode):
        raise ValueError("remote file type changed")
    files = receipt.get("files", {receipt["filename"]: receipt["binary_sha256"]})
    if any(not NAME.fullmatch(name) for name in files):
        raise ValueError("invalid transfer filename")
    if set(sftp.listdir(directory)) != set(files):
        raise ValueError("remote directory contains unowned files")
    for filename, expected in files.items():
        target = posixpath.join(directory, filename)
        if not stat.S_ISREG(sftp.lstat(target).st_mode) or remote_digest(sftp, target) != expected:
            raise ValueError("remote file changed; refusing cleanup")
    for filename in files:
        sftp.remove(posixpath.join(directory, filename))
    sftp.rmdir(directory)
    return {"state": "cleaned", "remote_directory": directory}
