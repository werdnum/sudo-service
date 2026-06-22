from __future__ import annotations

import json
import subprocess
import tempfile
import threading
import time
import unittest
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
CLI = ROOT / "sudo-service"


class FakeSudoServiceHandler(BaseHTTPRequestHandler):
    request_bodies: list[dict] = []
    auth_headers: list[str] = []
    phases: list[str] = ["Executed"]
    denial_reason = ""
    output = b""
    exit_code: int | None = None
    status_calls = 0
    require_rotated_token_after_first_status = False

    def log_message(self, _fmt: str, *_args: object) -> None:
        return

    def authenticated(self) -> bool:
        header = self.headers.get("Authorization", "")
        self.auth_headers.append(header)
        expected = "Bearer test-token"
        if self.require_rotated_token_after_first_status:
            if self.path == "/requests/uid-1" and self.status_calls > 0:
                expected = "Bearer rotated-token"
            elif self.path == "/requests/uid-1/output":
                expected = "Bearer rotated-token"
        return header == expected

    def do_POST(self) -> None:
        if self.path != "/requests":
            self.send_error(HTTPStatus.NOT_FOUND)
            return
        if not self.authenticated():
            self.send_error(HTTPStatus.UNAUTHORIZED)
            return
        length = int(self.headers.get("Content-Length", "0"))
        self.request_bodies.append(json.loads(self.rfile.read(length)))
        self.write_json({"uid": "uid-1", "name": "http-abc"})

    def do_GET(self) -> None:
        if not self.authenticated():
            self.send_error(HTTPStatus.UNAUTHORIZED)
            return
        if self.path == "/requests/uid-1":
            index = min(self.status_calls, len(self.phases) - 1)
            phase = self.phases[index]
            type(self).status_calls += 1
            body = {
                "uid": "uid-1",
                "name": "http-abc",
                "phase": phase,
            }
            if phase in {"Executed", "Failed"} and self.output:
                body["outputSecretRef"] = "out-1"
            if phase == "Failed":
                body["exitCode"] = self.exit_code
                if self.failure_reason:
                    body["failureReason"] = self.failure_reason
            if phase == "Denied":
                body["denialReason"] = self.denial_reason
            self.write_json(body)
            return
        if self.path == "/requests/uid-1/output":
            self.send_response(HTTPStatus.OK)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.send_header("Content-Length", str(len(self.output)))
            self.end_headers()
            self.wfile.write(self.output)
            return
        self.send_error(HTTPStatus.NOT_FOUND)

    def write_json(self, body: dict) -> None:
        payload = json.dumps(body).encode()
        self.send_response(HTTPStatus.OK)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


class SudoServiceCLITest(unittest.TestCase):
    def setUp(self) -> None:
        FakeSudoServiceHandler.request_bodies = []
        FakeSudoServiceHandler.auth_headers = []
        FakeSudoServiceHandler.phases = ["Executed"]
        FakeSudoServiceHandler.denial_reason = ""
        FakeSudoServiceHandler.failure_reason = ""
        FakeSudoServiceHandler.output = b""
        FakeSudoServiceHandler.exit_code = None
        FakeSudoServiceHandler.status_calls = 0
        FakeSudoServiceHandler.require_rotated_token_after_first_status = False
        self.tmp = tempfile.TemporaryDirectory()
        self.token_file = Path(self.tmp.name) / "token"
        self.token_file.write_text("test-token\n")
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), FakeSudoServiceHandler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self) -> None:
        self.server.shutdown()
        self.thread.join(timeout=5)
        self.server.server_close()
        self.tmp.cleanup()

    def run_cli(self, *args: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [
                str(CLI),
                "--url",
                f"http://127.0.0.1:{self.server.server_port}",
                "--token-file",
                str(self.token_file),
                *args,
            ],
            check=False,
            text=True,
            capture_output=True,
        )

    def test_creates_request_waits_and_prints_output(self) -> None:
        FakeSudoServiceHandler.phases = ["Pending", "Executed"]
        FakeSudoServiceHandler.output = b"pod/open-webui\n"

        result = self.run_cli(
            "--reason",
            "inspect stuck workload",
            "--image",
            "alpine/k8s:test",
            "--ttl-seconds-after-approval",
            "30",
            "--poll-interval",
            "0.01",
            "--quiet",
            "--",
            "kubectl",
            "get",
            "pod",
            "open-webui",
            "-n",
            "ml-bot",
            "-o",
            "name",
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "pod/open-webui\n")
        self.assertEqual(result.stderr, "")
        self.assertGreaterEqual(FakeSudoServiceHandler.status_calls, 2)
        self.assertEqual(
            FakeSudoServiceHandler.request_bodies,
            [
                {
                    "reason": "inspect stuck workload",
                    "command": "kubectl get pod open-webui -n ml-bot -o name",
                    "image": "alpine/k8s:test",
                    "ttlSecondsAfterApproval": 30,
                }
            ],
        )

    def test_failed_request_prints_output_and_exits_with_command_code(self) -> None:
        FakeSudoServiceHandler.phases = ["Failed"]
        FakeSudoServiceHandler.exit_code = 7
        FakeSudoServiceHandler.output = b"kubectl error\n"

        result = self.run_cli("--reason", "test failure", "--quiet", "--command", "kubectl get missing")

        self.assertEqual(result.returncode, 7)
        self.assertEqual(result.stdout, "kubectl error\n")
        self.assertEqual(result.stderr, "")

    def test_failed_request_without_output_surfaces_failure_reason(self) -> None:
        FakeSudoServiceHandler.phases = ["Failed"]
        FakeSudoServiceHandler.failure_reason = (
            "Executor Job sudo-exec-abc disappeared before controller observed completion"
        )

        # --quiet suppresses progress, but the failure reason is the only
        # explanation for a no-output Failed, so it must still surface.
        result = self.run_cli("--reason", "test failure", "--quiet", "--command", "kubectl get nodes")

        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout, "")
        self.assertIn("failed before output was available", result.stderr)
        self.assertIn("sudo-exec-abc disappeared", result.stderr)

    def test_rereads_projected_token_between_polls_and_output_fetch(self) -> None:
        FakeSudoServiceHandler.phases = ["Pending", "Executed"]
        FakeSudoServiceHandler.output = b"node/take5\n"
        FakeSudoServiceHandler.require_rotated_token_after_first_status = True

        process = subprocess.Popen(
            [
                str(CLI),
                "--url",
                f"http://127.0.0.1:{self.server.server_port}",
                "--token-file",
                str(self.token_file),
                "--reason",
                "test token rotation",
                "--poll-interval",
                "0.2",
                "--quiet",
                "--command",
                "kubectl get nodes -o name",
            ],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        deadline = time.time() + 5
        while FakeSudoServiceHandler.status_calls < 1 and time.time() < deadline:
            time.sleep(0.01)
        self.assertEqual(FakeSudoServiceHandler.status_calls, 1)
        self.token_file.write_text("rotated-token\n")
        stdout, stderr = process.communicate(timeout=5)

        self.assertEqual(process.returncode, 0, stderr)
        self.assertEqual(stdout, "node/take5\n")
        self.assertEqual(stderr, "")
        self.assertEqual(
            FakeSudoServiceHandler.auth_headers,
            [
                "Bearer test-token",
                "Bearer test-token",
                "Bearer rotated-token",
                "Bearer rotated-token",
            ],
        )

    def test_invalid_syntax_is_rejected_before_submitting(self) -> None:
        result = self.run_cli(
            "--reason",
            "read a secret",
            "--quiet",
            "--command",
            "kubectl get secret foo -o jsonpath='{.data.password}",  # unterminated quote
        )

        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout, "")
        self.assertIn("local syntax check", result.stderr)
        # The request must never reach the server.
        self.assertEqual(FakeSudoServiceHandler.request_bodies, [])

    def test_no_validate_flag_skips_local_syntax_check(self) -> None:
        FakeSudoServiceHandler.phases = ["Executed"]
        FakeSudoServiceHandler.output = b"ok\n"

        result = self.run_cli(
            "--reason",
            "skip validation",
            "--quiet",
            "--no-validate",
            "--poll-interval",
            "0.01",
            "--command",
            "kubectl get secret foo -o jsonpath='{.data.password}",  # unterminated quote
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(FakeSudoServiceHandler.request_bodies), 1)

    def test_denied_request_surfaces_denial_reason(self) -> None:
        FakeSudoServiceHandler.phases = ["Denied"]
        FakeSudoServiceHandler.denial_reason = "too broad"

        result = self.run_cli("--reason", "test denial", "--quiet", "--command", "kubectl delete pods --all")

        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout, "")
        self.assertIn("request uid-1 denied: too broad", result.stderr)

    def test_namespace_stdin_and_no_cluster_admin_flags(self) -> None:
        FakeSudoServiceHandler.phases = ["Executed"]
        FakeSudoServiceHandler.output = b"applied\n"

        with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as fh:
            fh.write("kind: Job\n")
            stdin_path = fh.name

        result = self.run_cli(
            "--reason",
            "apply a manifest without a heredoc",
            "--quiet",
            "--poll-interval",
            "0.01",
            "--namespace",
            "seaweedfs",
            "--stdin-file",
            stdin_path,
            "--no-cluster-admin",
            "--command",
            "kubectl apply -f -",
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            FakeSudoServiceHandler.request_bodies,
            [
                {
                    "reason": "apply a manifest without a heredoc",
                    "command": "kubectl apply -f -",
                    "namespace": "seaweedfs",
                    "stdin": "kind: Job\n",
                    "privileges": {"clusterAdmin": False},
                }
            ],
        )

    def test_image_pull_secret_flag_is_repeatable(self) -> None:
        FakeSudoServiceHandler.phases = ["Executed"]
        FakeSudoServiceHandler.output = b"ok\n"

        result = self.run_cli(
            "--reason",
            "pull a private image",
            "--quiet",
            "--poll-interval",
            "0.01",
            "--image",
            "registry.internal/private:1.0",
            "--image-pull-secret",
            "registry-creds",
            "--image-pull-secret",
            "backup-registry-creds",
            "--command",
            "kubectl get nodes",
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            FakeSudoServiceHandler.request_bodies,
            [
                {
                    "reason": "pull a private image",
                    "command": "kubectl get nodes",
                    "image": "registry.internal/private:1.0",
                    "imagePullSecrets": [
                        {"name": "registry-creds"},
                        {"name": "backup-registry-creds"},
                    ],
                }
            ],
        )


if __name__ == "__main__":
    unittest.main()
