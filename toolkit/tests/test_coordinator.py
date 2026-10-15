from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request

from artex_toolkit.coordinator import Coordinator, make_server, origin


class CoordinatorTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.seen = []
        self.peak = self.active = 0
        self.guard = threading.Lock()
        owner = self

        class Fixture(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_GET(self):
                with owner.guard:
                    owner.seen.append((self.path, self.headers.get("Authorization", "")))
                    owner.active += 1
                    owner.peak = max(owner.peak, owner.active)
                try:
                    status, data = 200, {"path": self.path}
                    if self.path == "/whoami":
                        token = self.headers.get("Authorization")
                        data = {"id": "A" if token == "fresh-A" else "B" if token == "fresh-B" else "anonymous"}
                    elif self.path == "/refresh":
                        data = {"token": "fresh-A"}
                    elif self.path == "/private":
                        data = {"owner": "A", "value": 7}  # Deliberately vulnerable synthetic fixture.
                    elif self.path == "/fixed":
                        status = 200 if self.headers.get("Authorization") == "fresh-A" else 403
                        data = {"owner": "A"} if status == 200 else {"error": "forbidden"}
                    elif self.path == "/soft404":
                        data = {"error": "not found"}
                    elif self.path == "/busy":
                        status = 429
                    elif self.path == "/slow":
                        time.sleep(0.05)
                    elif self.path == "/redirect":
                        self.send_response(302)
                        self.send_header("Location", "http://127.0.0.1:1/outside")
                        self.end_headers()
                        return
                    elif self.path == "/object":
                        data = {"id": "a/b", "csrf": "fixture-csrf"}
                    elif self.path == "/cookies":
                        data = {"cookie_seen": self.headers.get("Cookie", "")}
                    self.send_response(status)
                    self.send_header("Set-Cookie", "fixture-session=server-assigned; Path=/")
                    self.end_headers()
                    self.wfile.write(json.dumps(data).encode())
                finally:
                    with owner.guard:
                        owner.active -= 1

        self.target = ThreadingHTTPServer(("127.0.0.1", 0), Fixture)
        self.thread = threading.Thread(target=self.target.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.stop_target)
        self.url = f"http://127.0.0.1:{self.target.server_port}"
        self.config = {"token": "t" * 32, "origins": {self.url: {"requests": 100, "rate": 1000, "concurrency": 2}},
                       "identities": {}}
        for name in ("A", "B"):
            self.config["identities"][name] = {
                "origins": [self.url], "headers": {"Authorization": "fresh-" + name},
                "check": {"request": {"url": self.url + "/whoami"}, "expect": {"status": 200, "json": {"/id": name}}}}
        self.coordinator = Coordinator(self.config, self.root / "state")
        self.addCleanup(lambda: self.coordinator.close())

    def stop_target(self):
        self.target.shutdown()
        self.target.server_close()
        self.thread.join()

    def plan(self, path="/private", identity="A"):
        return {"test": "ownership", "revision": "fixture-1", "steps": [
            {"identity": identity, "role": "proof", "request": {"url": self.url + path},
             "expect": {"status": 200, "json": {"/owner": "A"}}}]}

    def test_identity_matrix_and_expired_session_are_distinct(self):
        plan = self.plan(identity="B")
        plan["steps"].insert(0, {**self.plan()["steps"][0], "role": "baseline"})
        result = self.coordinator.run(plan)
        self.assertEqual(result["state"], "observed")
        self.assertEqual([x["role"] for x in result["evidence"]], ["identity_check", "baseline", "identity_check", "proof"])
        fixed = self.plan("/fixed", "B")
        fixed["steps"][0]["expect"] = {"status": 403, "json": {"/error": "forbidden"}}
        self.assertEqual(self.coordinator.run(fixed)["state"], "observed")
        self.config["identities"]["expired"] = {**self.config["identities"]["A"], "headers": {"Authorization": "old"}}
        expired = self.coordinator.run(self.plan(identity="expired"))
        self.assertEqual(expired["state"], "inconclusive")
        self.assertEqual(len(expired["evidence"]), 1)

    def test_refresh_and_redaction(self):
        cfg = self.config["identities"]["A"]
        cfg["headers"]["Authorization"] = "expired-secret"
        cfg["refresh"] = {"request": {"url": self.url + "/refresh"}, "capture_headers": {"Authorization": "/token"}}
        result = self.coordinator.run(self.plan())
        self.assertEqual(result["state"], "observed")
        self.assertNotIn("fresh-A", json.dumps(result))
        self.assertNotIn("expired-secret", json.dumps(result))
        self.assertEqual(len(result["evidence"]), 4)

    def test_soft404_requires_semantic_match_and_redirects_never_follow(self):
        result = self.coordinator.run(self.plan("/soft404"))
        self.assertEqual(result["state"], "not_observed")
        identity, _ = self.coordinator.identity("A")
        response = self.coordinator.request({"url": self.url + "/redirect"}, identity)
        self.assertEqual(response[0]["status"], 302)
        with self.assertRaisesRegex(ValueError, "identity cannot"):
            self.coordinator.request({"url": "http://127.0.0.1:1/whoami"}, identity)

    def test_parallel_request_budget_is_aggregate_and_persistent(self):
        policy = self.coordinator.origins[origin(self.url)]
        policy["requests"] = 7
        identity, _ = self.coordinator.identity("anonymous")
        def run(_):
            try:
                self.coordinator.request({"url": self.url + "/slow"}, identity)
                return True
            except ValueError:
                return False
        with ThreadPoolExecutor(max_workers=12) as pool:
            results = list(pool.map(run, range(16)))
        self.assertEqual(sum(results), 7)
        self.assertLessEqual(self.peak, 2)
        self.coordinator.close()
        self.coordinator = Coordinator(self.config, self.root / "state")
        self.assertFalse(run(0))

    def test_workflow_dedup_restart_and_callback_survive_runner(self):
        plan = self.plan()
        with ThreadPoolExecutor(max_workers=4) as pool:
            results = list(pool.map(lambda _: self.coordinator.run(plan), range(4)))
        ids = {r["id"] for r in results}
        self.assertEqual(len(ids), 1)
        self.assertEqual(sum(path == "/private" for path, _ in self.seen), 1)
        callback = self.coordinator.callback_new(next(iter(ids)), 60)
        self.coordinator.close()
        self.coordinator = Coordinator(self.config, self.root / "state")
        self.assertTrue(self.coordinator.callback_hit(callback["id"], "GET", b"delayed synthetic callback"))
        status = self.coordinator.status(next(iter(ids)))
        self.assertEqual(len(status["callbacks"][0]["events"]), 1)
        self.assertTrue(self.coordinator.run(plan)["reused"])
        self.coordinator.db.execute("UPDATE callbacks SET expires=0")
        self.coordinator.db.commit()
        self.assertFalse(self.coordinator.callback_hit(callback["id"], "GET", b""))

    def test_stateful_capture_and_encoded_replay(self):
        plan = {"test": "stateful", "revision": "1", "steps": [
            {"request": {"url": self.url + "/object"}, "capture": {"object_id": "/id"}},
            {"request": {"url": self.url + "/object/{{object_id|url}}"}, "expect": {"json": {"/path": "/object/a%2Fb"}}}]}
        self.assertEqual(self.coordinator.run(plan)["state"], "observed")

    def test_anonymous_never_inherits_a_session_and_cannot_override_identity(self):
        self.config["identities"]["anonymous"] = {"headers": {"Authorization": "must-not-send"}}
        anonymous, _ = self.coordinator.identity("anonymous")
        self.coordinator.request({"url": self.url + "/cookies"}, anonymous)
        self.assertEqual(self.coordinator.request({"url": self.url + "/cookies"}, anonymous)[1]["cookie_seen"], "")
        self.assertFalse(any(token for _, token in self.seen))
        named, _ = self.coordinator.identity("A")
        self.coordinator.request({"url": self.url + "/cookies"}, named)
        self.assertEqual(self.coordinator.request({"url": self.url + "/cookies"}, named)[1]["cookie_seen"], "fixture-session=server-assigned")
        with self.assertRaisesRegex(ValueError, "identity reference"):
            self.coordinator.request({"url": self.url, "headers": {"Authorization": "other"}}, named)
        with self.assertRaisesRegex(ValueError, "identity reference"):
            self.coordinator.request({"url": self.url, "headers": {"Cookie": "session=other"}}, anonymous)

    def test_coordinator_authentication_and_single_owner(self):
        with self.assertRaisesRegex(ValueError, "owner"):
            Coordinator(self.config, self.root / "state")
        server = make_server(self.coordinator, ("127.0.0.1", 0))
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            url = f"http://127.0.0.1:{server.server_port}/runs"
            with self.assertRaises(urllib.error.HTTPError) as result:
                urllib.request.urlopen(urllib.request.Request(url, data=b"{}"))
            self.assertEqual(result.exception.code, 401)
            invalid = urllib.request.Request(url, data=b"[]", headers={"Authorization": "Bearer " + self.config["token"]})
            with self.assertRaises(urllib.error.HTTPError) as result:
                urllib.request.urlopen(invalid)
            self.assertEqual(result.exception.code, 400)
            req = urllib.request.Request(url, data=json.dumps(self.plan()).encode(), headers={"Authorization": "Bearer " + self.config["token"]})
            with urllib.request.urlopen(req) as result:
                self.assertEqual(json.load(result)["state"], "observed")
        finally:
            server.shutdown()
            server.server_close()
            thread.join()

    def test_invalid_or_assertionless_plans_have_no_network_effects(self):
        for invalid in ([], {"test": "x", "revision": "1", "steps": "wrong"},
                        {"test": "x", "revision": "1", "steps": [None]},
                        {"test": "x", "revision": "1", "steps": [{"request": {"url": self.url}}]},
                        {"test": "x", "revision": "1", "steps": [{"request": {"url": self.url}, "expect": {"typo": 200}}]}):
            with self.assertRaises(ValueError):
                self.coordinator.run(invalid)
        self.assertEqual(self.seen, [])
        self.assertEqual(self.coordinator.db.execute("SELECT count(*) FROM runs").fetchone()[0], 0)

    def test_throttling_and_history_limit_never_claim_success(self):
        plan = self.plan("/busy", "anonymous")
        result = self.coordinator.run(plan)
        self.assertEqual(result["state"], "inconclusive")
        self.assertEqual(result["evidence"][0]["status"], 429)
        self.config["max_runs"] = 1
        with self.assertRaisesRegex(ValueError, "history limit"):
            self.coordinator.run(self.plan())
        self.assertEqual(len(self.seen), 1)
