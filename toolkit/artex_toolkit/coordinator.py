"""Persistent verification coordinator, shared by a project's disposable runners.

One process owns a private local SQLite database. All HTTP steps, including
identity checks and refreshes, consume the same origin budgets. This is not a
transparent network firewall for arbitrary third-party tools.
"""
from contextlib import contextmanager
import hashlib
import http.cookiejar
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import secrets
import sqlite3
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

MAX_BODY = 1024 * 1024
SENSITIVE = re.compile(r"password|passwd|secret|token|authorization|cookie|api[_-]?key", re.I)


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def origin(url):
    p = urllib.parse.urlsplit(url)
    if p.scheme not in ("http", "https") or not p.hostname or p.username or p.password or p.fragment:
        raise ValueError("expected an HTTP(S) URL without embedded credentials or fragment")
    host = p.hostname.lower()
    if ":" in host:
        host = "[" + host + "]"
    port = p.port or (443 if p.scheme == "https" else 80)
    return f"{p.scheme}://{host}:{port}"


def pointer(data, path):
    if path == "":
        return data
    if not path.startswith("/"):
        raise ValueError("JSON pointer must begin with /")
    for key in path[1:].split("/"):
        key = key.replace("~1", "/").replace("~0", "~")
        data = data[int(key)] if isinstance(data, list) else data[key]
    return data


def redact(data):
    if isinstance(data, dict):
        return {k: "[redacted]" if SENSITIVE.search(k) else redact(v) for k, v in data.items()}
    if isinstance(data, list):
        return [redact(v) for v in data]
    return data


def safe_url(url):
    p = urllib.parse.urlsplit(url)
    query = urllib.parse.urlencode([(k, "[redacted]" if SENSITIVE.search(k) else v)
                                   for k, v in urllib.parse.parse_qsl(p.query, keep_blank_values=True)])
    return urllib.parse.urlunsplit((p.scheme, p.netloc, p.path, query, ""))


def render(value, variables):
    if isinstance(value, dict):
        return {k: render(v, variables) for k, v in value.items()}
    if isinstance(value, list):
        return [render(v, variables) for v in value]
    if not isinstance(value, str):
        return value
    def replacement(m):
        name, escaped = m.group(1), m.group(2)
        if name not in variables:
            raise ValueError("missing captured variable: " + name)
        val = str(variables[name])
        return urllib.parse.quote(val, safe="") if escaped else val
    return re.sub(r"\{\{([A-Za-z0-9_]+)(\|url)?\}\}", replacement, value)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


class Coordinator:
    def __init__(self, config, directory):
        self.config = config
        if len(config.get("token", "")) < 32:
            raise ValueError("coordinator token must contain at least 32 characters")
        self.origins = {origin(url): policy for url, policy in config["origins"].items()}
        if not self.origins:
            raise ValueError("at least one origin budget is required")
        for p in self.origins.values():
            for k, default in [("requests", 1000), ("concurrency", 2), ("rate", 5), ("window_seconds", 3600)]:
                p.setdefault(k, default)
                if not isinstance(p[k], (int, float)) or not 0 < p[k] <= 1000000:
                    raise ValueError("invalid budget " + k)
        self.directory = Path(directory)
        self.directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        if self.directory.is_symlink() or self.directory.stat().st_mode & 0o077:
            raise ValueError("coordinator state requires a private directory (0700)")
        # A singleton writer avoids independent rate limiters claiming to enforce
        # one aggregate policy. OS releases this lock on crash.
        import fcntl
        self.owner = (self.directory / "owner.lock").open("a+")
        try:
            fcntl.flock(self.owner, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            self.owner.close()
            raise ValueError("coordinator state already has an owner") from None
        db_path = self.directory / "state.sqlite"
        fd = os.open(db_path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        os.close(fd)
        self.db = sqlite3.connect(db_path, check_same_thread=False)
        self.db.executescript("""
          PRAGMA journal_mode=WAL;
          CREATE TABLE IF NOT EXISTS budgets (origin TEXT PRIMARY KEY, window REAL, used INTEGER, next REAL);
          CREATE TABLE IF NOT EXISTS runs (id TEXT PRIMARY KEY, fingerprint TEXT, created REAL, state TEXT, result TEXT);
          CREATE INDEX IF NOT EXISTS runs_fingerprint ON runs(fingerprint, created);
          CREATE TABLE IF NOT EXISTS callbacks (id TEXT PRIMARY KEY, run_id TEXT, expires REAL, events TEXT);
        """)
        self.db.execute("UPDATE runs SET state='inconclusive', result=? WHERE state='running'",
                        (canonical({"state": "inconclusive", "reason": "coordinator restarted; review evidence before retrying"}),))
        self.db.commit()
        self.cv = threading.Condition(threading.RLock())
        self.active = {}
        self.identities = {}
        self.identity_locks = {}
        self.closed = False

    def close(self):
        with self.cv:
            self.closed = True
            self.cv.notify_all()
            self.db.close()
            self.owner.close()

    @contextmanager
    def budget(self, target):
        if target not in self.origins:
            raise ValueError("origin is outside configured execution scope")
        p = self.origins[target]
        deadline = time.monotonic() + 30
        with self.cv:
            while True:
                if self.closed:
                    raise ValueError("coordinator stopping")
                now = time.time()
                row = self.db.execute("SELECT window, used, next FROM budgets WHERE origin=?", (target,)).fetchone()
                window, used, next_at = row or (now, 0, now)
                if now - window >= p["window_seconds"]:
                    window, used = now, 0
                if used >= p["requests"]:
                    raise ValueError("target request budget exhausted")
                if self.active.get(target, 0) < p["concurrency"] and now >= next_at:
                    self.active[target] = self.active.get(target, 0) + 1
                    self.db.execute("INSERT OR REPLACE INTO budgets VALUES(?,?,?,?)",
                                    (target, window, used + 1, now + 1 / p["rate"]))
                    self.db.commit()
                    break
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise ValueError("target budget wait timed out")
                self.cv.wait(min(max(next_at - now, 0.05), remaining, 1))
        try:
            yield
        finally:
            with self.cv:
                self.active[target] -= 1
                self.cv.notify_all()

    def identity(self, name):
        with self.cv:
            if name not in self.identities:
                if name != "anonymous" and name not in self.config.get("identities", {}):
                    raise ValueError("unknown identity reference")
                cfg = {} if name == "anonymous" else self.config.get("identities", {}).get(name, {})
                jar = http.cookiejar.CookieJar()
                handlers = [urllib.request.ProxyHandler({}), NoRedirect()]
                if name != "anonymous":
                    handlers.append(urllib.request.HTTPCookieProcessor(jar))
                opener = urllib.request.build_opener(*handlers)
                self.identities[name] = (cfg, dict(cfg.get("headers", {})), opener)
                self.identity_locks[name] = threading.RLock()
            return self.identities[name], self.identity_locks[name]

    def request(self, spec, identity):
        cfg, headers, opener = identity
        url = spec["url"]
        target = origin(url)
        if cfg and target not in {origin(u) for u in cfg.get("origins", [])}:
            raise ValueError("identity cannot be sent to this origin")
        method = spec.get("method", "GET").upper()
        if method not in ("GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"):
            raise ValueError("unsupported HTTP method")
        merged = {**headers, **spec.get("headers", {})}
        if any(k.lower() in ("authorization", "cookie", "x-api-key") for k in spec.get("headers", {})):
            raise ValueError("use an identity reference instead of overriding its credentials")
        # These are generated by the HTTP transport, never user-controlled framing.
        if any(k.lower() in ("host", "content-length", "transfer-encoding", "connection", "proxy-authorization") for k in merged):
            raise ValueError("transport-managed header")
        body = spec.get("body")
        if "json" in spec:
            body = canonical(spec["json"])
            merged["Content-Type"] = "application/json"
        raw_body = body.encode() if isinstance(body, str) else None
        if raw_body is not None and len(raw_body) > MAX_BODY:
            raise ValueError("request body too large")
        req = urllib.request.Request(url, data=raw_body, headers=merged, method=method)
        started = time.monotonic()
        with self.budget(target):
            try:
                response = opener.open(req, timeout=min(max(float(spec.get("timeout", 10)), 0.1), 30))
            except urllib.error.HTTPError as e:
                response = e
            with response:
                data = response.read(MAX_BODY + 1)
                status = response.code
                if status in (429, 503):
                    try:
                        delay = min(max(float(response.headers.get("Retry-After", "1")), 1), 60)
                    except ValueError:
                        delay = 1
                    with self.cv:
                        self.db.execute("UPDATE budgets SET next=MAX(next,?) WHERE origin=?", (time.time() + delay, target))
                        self.db.commit()
        if len(data) > MAX_BODY:
            raise ValueError("response body exceeds capture limit")
        text = data.decode("utf-8", errors="replace")
        try:
            parsed = json.loads(text)
        except ValueError:
            parsed = None
        preview = canonical(redact(parsed)) if parsed is not None else text
        for key, value in merged.items():
            if SENSITIVE.search(key) and value:
                preview = preview.replace(value, "[redacted]")
        evidence = {"method": method, "url": safe_url(url), "status": status,
                    "response_sha256": hashlib.sha256(data).hexdigest(), "bytes": len(data),
                    "preview": preview[:4096], "elapsed_ms": round((time.monotonic() - started) * 1000)}
        return evidence, parsed, text

    @staticmethod
    def matches(expect, response):
        evidence, parsed, text = response
        try:
            if "status" in expect and evidence["status"] != expect["status"]:
                return False
            if "contains" in expect and expect["contains"] not in text:
                return False
            return all(pointer(parsed, path) == value for path, value in expect.get("json", {}).items())
        except (TypeError, KeyError, IndexError, ValueError):
            return False

    def verify_identity(self, name, identity, records):
        if name == "anonymous":
            return
        cfg, headers, _ = identity
        check = cfg.get("check")
        if not check or not check.get("expect"):
            raise ValueError("identity needs a semantic session check")
        for attempt in range(2):
            response = self.request(check["request"], identity)
            records.append({"role": "identity_check", **response[0]})
            if response[0]["status"] < 300 and self.matches(check["expect"], response):
                return
            refresh = cfg.get("refresh")
            if attempt or not refresh:
                break
            response = self.request(refresh["request"], identity)
            records.append({"role": "identity_refresh", **response[0]})
            if response[0]["status"] >= 400 or not self.matches(refresh.get("expect", {}), response):
                break
            for header, path in refresh.get("capture_headers", {}).items():
                headers[header] = str(pointer(response[1], path))
        raise ValueError("identity session is not valid")

    def run(self, plan):
        if not isinstance(plan, dict):
            raise ValueError("workflow must be an object")
        steps = plan.get("steps", [])
        if not isinstance(steps, list) or not 1 <= len(steps) <= 30 or len(canonical(plan)) > 128 * 1024:
            raise ValueError("expected 1..30 bounded workflow steps")
        if not plan.get("test") or not plan.get("revision"):
            raise ValueError("test name and revision are required for coverage tracking")
        assertions = False
        for step in steps:
            if not isinstance(step, dict) or not isinstance(step.get("request"), dict):
                raise ValueError("each step needs a request object")
            expect = step.get("expect", {})
            if not isinstance(expect, dict) or set(expect) - {"status", "contains", "json"}:
                raise ValueError("unknown or invalid response assertion")
            if "json" in expect and not isinstance(expect["json"], dict):
                raise ValueError("JSON assertions must map pointers to expected values")
            assertions |= "status" in expect or bool(expect.get("contains")) or bool(expect.get("json"))
        if not assertions:
            raise ValueError("workflow needs at least one response assertion")
        if not isinstance(plan.get("variables", {}), dict):
            raise ValueError("variables must be an object")
        fingerprint = hashlib.sha256(canonical({
            "plan": {k: v for k, v in plan.items() if k != "repeat"},
            "conditions": {k: self.config.get(k) for k in ("origins", "identities")},
        }).encode()).hexdigest()
        now = time.time()
        with self.cv:
            old = self.db.execute("SELECT id,state,result FROM runs WHERE fingerprint=? ORDER BY created DESC LIMIT 1", (fingerprint,)).fetchone()
            if old and not plan.get("repeat"):
                return {"id": old[0], "state": old[1], "reused": True, "result": json.loads(old[2]) if old[2] else None}
            if self.db.execute("SELECT count(*) FROM runs").fetchone()[0] >= self.config.get("max_runs", 10000):
                raise ValueError("verification history limit reached; export and rotate coordinator state")
            run_id = secrets.token_hex(16)
            self.db.execute("INSERT INTO runs VALUES(?,?,?,?,NULL)", (run_id, fingerprint, now, "running"))
            self.db.commit()
        records, variables = [], dict(plan.get("variables", {}))
        result = {"id": run_id, "test": plan["test"], "revision": plan["revision"], "state": "observed", "evidence": records}
        checked = set()
        try:
            for step in steps:
                name = step.get("identity", "anonymous")
                identity, lock = self.identity(name)
                with lock:
                    if name not in checked:
                        self.verify_identity(name, identity, records)
                        checked.add(name)
                    response = self.request(render(step["request"], variables), identity)
                    records.append({"role": step.get("role", "supporting"), "identity": name, **response[0]})
                    if response[0]["status"] in (401, 429) or response[0]["status"] >= 500:
                        raise ValueError("authentication, throttling or server failure prevents comparison")
                    if not self.matches(render(step.get("expect", {}), variables), response):
                        if name != "anonymous":
                            self.verify_identity(name, identity, records)
                        if step.get("role") == "baseline" or response[0]["status"] == 403 or 300 <= response[0]["status"] < 400:
                            raise ValueError("baseline, redirect or access denial prevents comparison")
                        result["state"] = "not_observed"
                        break
                    for key, path in step.get("capture", {}).items():
                        variables[key] = pointer(response[1], path)
        except Exception as e:
            # Network exceptions can include URLs/credentials; do not serialize
            # their raw repr into an agent-visible result.
            result["state"] = "inconclusive"
            result["reason"] = str(e) if isinstance(e, ValueError) else type(e).__name__
        with self.cv:
            self.db.execute("UPDATE runs SET state=?,result=? WHERE id=?", (result["state"], canonical(result), run_id))
            self.db.commit()
        return result

    def callback_new(self, run_id, ttl=3600):
        ttl = int(ttl)
        if not 1 <= ttl <= 86400:
            raise ValueError("callback TTL must be 1..86400 seconds")
        with self.cv:
            if not self.db.execute("SELECT 1 FROM runs WHERE id=?", (run_id,)).fetchone():
                raise ValueError("callback must reference an existing verification run")
            self.db.execute("DELETE FROM callbacks WHERE expires < ?", (time.time() - 86400,))
            if self.db.execute("SELECT count(*) FROM callbacks").fetchone()[0] >= 10000:
                raise ValueError("callback limit reached")
            token = secrets.token_hex(32)
            expires = time.time() + ttl
            self.db.execute("INSERT INTO callbacks VALUES(?,?,?,?)", (token, run_id, expires, "[]"))
            self.db.commit()
        base = self.config.get("callback_base", "").rstrip("/")
        return {"id": token, "run_id": run_id, "expires": expires, "url": base + "/callback/" + token if base else None}

    def callback_hit(self, token, method, body):
        with self.cv:
            row = self.db.execute("SELECT expires,events FROM callbacks WHERE id=?", (token,)).fetchone()
            if not row or row[0] < time.time():
                return False
            events = json.loads(row[1])
            if len(events) < 32:
                events.append({"time": time.time(), "method": method, "body_sha256": hashlib.sha256(body).hexdigest(), "bytes": len(body)})
                self.db.execute("UPDATE callbacks SET events=? WHERE id=?", (canonical(events), token))
                self.db.commit()
            return True

    def status(self, run_id):
        with self.cv:
            row = self.db.execute("SELECT state,result FROM runs WHERE id=?", (run_id,)).fetchone()
            if not row:
                raise ValueError("unknown run")
            callbacks = [{"id": r[0], "expires": r[1], "events": json.loads(r[2])}
                         for r in self.db.execute("SELECT id,expires,events FROM callbacks WHERE run_id=?", (run_id,))]
            return {"id": run_id, "state": row[0], "result": json.loads(row[1]) if row[1] else None, "callbacks": callbacks}


def make_server(coordinator, address):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass  # URLs can contain callback capabilities; never log them.

        def reply(self, status, obj):
            data = canonical(obj).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self):
            self.handle_api()

        def do_POST(self):
            self.handle_api()

        def handle_api(self):
            self.connection.settimeout(15)
            try:
                size = int(self.headers.get("Content-Length", "0"))
                if not 0 <= size <= 128 * 1024 or self.headers.get("Transfer-Encoding"):
                    return self.reply(413, {"error": "request too large or unsupported framing"})
                if self.path.startswith("/callback/"):
                    token = self.path.removeprefix("/callback/").split("?", 1)[0]
                    body = self.rfile.read(size)
                    ok = coordinator.callback_hit(token, self.command, body)
                    return self.reply(200 if ok else 404, {"accepted": ok})
                if not secrets.compare_digest(self.headers.get("Authorization", ""), "Bearer " + coordinator.config["token"]):
                    return self.reply(401, {"error": "authentication required"})
                if self.command == "GET" and self.path.startswith("/runs/"):
                    return self.reply(200, coordinator.status(self.path.removeprefix("/runs/")))
                obj = json.loads(self.rfile.read(size)) if size else {}
                if self.command == "POST" and self.path == "/runs":
                    return self.reply(200, coordinator.run(obj))
                if self.command == "POST" and self.path == "/callbacks":
                    return self.reply(200, coordinator.callback_new(obj["run_id"], obj.get("ttl", 3600)))
                self.reply(404, {"error": "unknown endpoint"})
            except (ValueError, KeyError, TypeError) as e:
                self.reply(400, {"error": str(e) if isinstance(e, ValueError) else "invalid request schema"})
            except (BrokenPipeError, ConnectionResetError, TimeoutError):
                pass
            except Exception:
                self.reply(500, {"error": "coordinator failure"})

    return ThreadingHTTPServer(address, Handler)
