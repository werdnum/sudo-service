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

    def write_request_file(self, contents: str, suffix: str) -> str:
        path = Path(self.tmp.name) / f"request{suffix}"
        path.write_text(contents)
        return str(path)

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

    def test_json_request_file_submits_every_supported_structured_field(self) -> None:
        body = {
            "reason": "recover one file",
            "command": "cp /source/file /work/file",
            "image": "busybox:1.37",
            "ttlSecondsAfterApproval": 120,
            "namespace": "storage",
            "stdin": "input\n",
            "env": [
                {"name": "PLAIN", "value": "value"},
                {"name": "TOKEN", "valueFrom": {"secretKeyRef": {"name": "creds", "key": "token"}}},
            ],
            "envFrom": [{"prefix": "APP_", "secretRef": {"name": "app-env"}}],
            "volumes": [
                {"name": "work", "emptyDir": {}},
                {"name": "source", "persistentVolumeClaim": {"claimName": "data", "readOnly": True}},
            ],
            "volumeMounts": [
                {"name": "work", "mountPath": "/work"},
                {"name": "source", "mountPath": "/source", "readOnly": True},
            ],
            "initContainers": [{
                "name": "stage",
                "image": "busybox:1.37",
                "command": ["/bin/sh", "-c"],
                "args": ["cp /bin/cp /work/cp"],
                "env": [{"name": "MODE", "value": "safe"}],
                "envFrom": [{"configMapRef": {"name": "settings"}}],
                "volumeMounts": [{"name": "work", "mountPath": "/work"}],
            }],
            "imagePullSecrets": [{"name": "registry-creds"}],
            "privileges": {"clusterAdmin": False},
        }
        path = self.write_request_file(json.dumps(body), ".json")

        result = self.run_cli("--request-file", path, "--quiet", "--poll-interval", "0.01")

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(FakeSudoServiceHandler.request_bodies, [body])

    def test_yaml_request_file_supports_rich_fields_and_block_command(self) -> None:
        yaml = (
            "reason: Recover one file without nested shell quoting\n"
            "command: |-\n"
            "  set -eu\n"
            "\n"
            "  # Preserve block scalar contents exactly\n"
            "  cp /source/file /work/file\n"
            "image: busybox:1.37\n"
            "ttlSecondsAfterApproval: 120\n"
            "namespace: storage\n"
            "env:\n"
            "  - name: PLAIN\n"
            "    value: value\n"
            "envFrom:\n"
            "  - secretRef: {name: app-env}\n"
            "volumes:\n"
            "  - name: work\n"
            "    emptyDir: {}\n"
            "  - name: source\n"
            "    persistentVolumeClaim: {claimName: data, readOnly: true}\n"
            "volumeMounts:\n"
            "  - {name: work, mountPath: /work}\n"
            "  - {name: source, mountPath: /source, readOnly: true}\n"
            "initContainers:\n"
            "  - name: stage\n"
            "    image: busybox:1.37\n"
            "    command: [/bin/sh, -c]\n"
            "    args: ['cp /bin/cp /work/cp']\n"
            "    volumeMounts: [{name: work, mountPath: /work}]\n"
            "imagePullSecrets:\n"
            "  - name: registry-creds\n"
            "privileges: {clusterAdmin: false}\n"
        )
        path = self.write_request_file(yaml, ".yaml")

        result = self.run_cli("--request-file", path, "--quiet", "--poll-interval", "0.01")

        self.assertEqual(result.returncode, 0, result.stderr)
        body = FakeSudoServiceHandler.request_bodies[0]
        self.assertEqual(
            body["command"],
            "set -eu\n\n# Preserve block scalar contents exactly\ncp /source/file /work/file",
        )
        self.assertEqual(body["env"], [{"name": "PLAIN", "value": "value"}])
        self.assertEqual(body["envFrom"], [{"secretRef": {"name": "app-env"}}])
        self.assertEqual(body["volumes"][1]["persistentVolumeClaim"]["readOnly"], True)
        self.assertEqual(body["initContainers"][0]["command"], ["/bin/sh", "-c"])
        self.assertEqual(body["privileges"], {"clusterAdmin": False})

    def test_request_file_can_take_stdin_from_a_separate_file(self) -> None:
        request_path = self.write_request_file(
            '{"reason":"apply manifest","command":"kubectl apply -f -"}', ".json"
        )
        stdin_path = Path(self.tmp.name) / "manifest.yaml"
        stdin_path.write_text("kind: ConfigMap\n")

        result = self.run_cli(
            "--request-file", request_path, "--stdin-file", str(stdin_path),
            "--quiet", "--poll-interval", "0.01",
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(FakeSudoServiceHandler.request_bodies[0]["stdin"], "kind: ConfigMap\n")

    def test_preview_prints_normalized_effective_request_before_submission(self) -> None:
        path = self.write_request_file(
            '{"command":"kubectl get nodes","reason":"inspect nodes",'
            '"env":[{"value":"b","name":"A"}]}',
            ".json",
        )

        result = self.run_cli(
            "--request-file", path, "--preview", "--quiet", "--poll-interval", "0.01"
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stderr), FakeSudoServiceHandler.request_bodies[0])
        self.assertLess(result.stderr.index('"command"'), result.stderr.index('"reason"'))

    def test_request_file_rejects_request_building_flags(self) -> None:
        path = self.write_request_file(
            '{"reason":"inspect","command":"kubectl get nodes"}', ".json"
        )
        flag_sets = (
            ("--reason", "override"),
            ("--command", "kubectl get pods"),
            ("--image", "busybox"),
            ("--namespace", "other"),
            ("--no-cluster-admin",),
            ("--image-pull-secret", "creds"),
            ("--ttl-seconds-after-approval", "30"),
            ("--", "kubectl", "get", "pods"),
        )
        for flags in flag_sets:
            with self.subTest(flags=flags):
                result = self.run_cli("--request-file", path, *flags)
                self.assertEqual(result.returncode, 2)
                self.assertIn("--request-file cannot be combined", result.stderr)
        self.assertEqual(FakeSudoServiceHandler.request_bodies, [])

    def test_request_file_rejects_duplicate_stdin_sources(self) -> None:
        path = self.write_request_file(
            '{"reason":"apply","command":"cat","stdin":"inline"}', ".json"
        )
        stdin_path = Path(self.tmp.name) / "stdin"
        stdin_path.write_text("external")

        result = self.run_cli("--request-file", path, "--stdin-file", str(stdin_path))

        self.assertEqual(result.returncode, 1)
        self.assertIn("cannot be combined with a request file that sets stdin", result.stderr)
        self.assertEqual(FakeSudoServiceHandler.request_bodies, [])

    def test_invalid_request_files_fail_before_submission(self) -> None:
        invalid = {
            "not-an-object": "[]",
            "missing-command": '{"reason":"why"}',
            "unknown-field": '{"reason":"why","command":"true","commnad":"false"}',
            "wrong-list-type": '{"reason":"why","command":"true","volumes":{}}',
            "bad-yaml": "reason: why\n  command: true\n",
            "unsafe-yaml-tag": (
                "reason: why\ncommand: true\nenv: "
                "!!python/object/apply:builtins.list [[]]\n"
            ),
            "non-string-key": "reason: why\ncommand: true\n1: value\n",
            "non-json-value": (
                "reason: why\ncommand: true\nenv:\n"
                "  - {name: WHEN, value: 2026-07-21}\n"
            ),
        }
        for name, contents in invalid.items():
            with self.subTest(name=name):
                path = self.write_request_file(contents, f"-{name}.yaml")
                result = self.run_cli("--request-file", path)
                self.assertEqual(result.returncode, 1)
                self.assertIn("sudo-service:", result.stderr)
        self.assertEqual(FakeSudoServiceHandler.request_bodies, [])

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
