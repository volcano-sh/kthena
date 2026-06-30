from __future__ import annotations

import select
import socket
import subprocess
import time


class K8sManager:
    """Manage Kubernetes resources needed by the benchmark."""

    ROUTER_NAMESPACE = "kthena-system"
    ROUTER_DEPLOYMENT = "kthena-router"
    ROUTER_SVC_PORT = 80
    DEFAULT_LOCAL_PORT = 8080
    MOCKER_DEPLOYMENT = "mocker-llm"

    def __init__(self, namespace: str = "default", local_port: int = DEFAULT_LOCAL_PORT):
        self.namespace = namespace
        self.local_port = local_port
        self._pf_proc: subprocess.Popen[str] | None = None

    def apply(self, manifest_path: str) -> None:
        subprocess.run(
            ["kubectl", "apply", "-f", manifest_path],
            check=True,
            capture_output=True,
            text=True,
        )
        time.sleep(5)

    def delete(self, manifest_path: str) -> None:
        subprocess.run(
            ["kubectl", "delete", "-f", manifest_path, "--ignore-not-found"],
            check=True,
            capture_output=True,
            text=True,
        )

    def apply_router_config(self, config_path: str) -> None:
        print(f"  Applying router config: {config_path}")
        self.apply(config_path)

        print(f"  Restarting router deployment {self.ROUTER_DEPLOYMENT}...")
        subprocess.run(
            [
                "kubectl",
                "rollout",
                "restart",
                f"deployment/{self.ROUTER_DEPLOYMENT}",
                "-n",
                self.ROUTER_NAMESPACE,
            ],
            check=True,
            capture_output=True,
            text=True,
        )

        print("  Waiting for router rollout to complete...")
        subprocess.run(
            [
                "kubectl",
                "rollout",
                "status",
                f"deployment/{self.ROUTER_DEPLOYMENT}",
                "-n",
                self.ROUTER_NAMESPACE,
                "--timeout=120s",
            ],
            check=True,
            capture_output=True,
            text=True,
        )

    def wait_for_backend_ready(self) -> None:
        print("  Waiting for mocker-llm to be ready...")
        subprocess.run(
            [
                "kubectl",
                "rollout",
                "status",
                f"deployment/{self.MOCKER_DEPLOYMENT}",
                "--timeout=120s",
            ],
            check=True,
            capture_output=True,
            text=True,
        )

    def wait_for_router_ready(self, mocker_model_name: str, endpoint: str, timeout: int = 300) -> None:
        """Probe the router to confirm the route table has the given model loaded."""
        print(f"  Probing router for model={mocker_model_name} at {endpoint}...")
        deadline = time.monotonic() + timeout
        request_body = (
            '{"model":"'
            + mocker_model_name
            + '","messages":[{"role":"user","content":"ping"}],"max_tokens":1}'
        )
        while time.monotonic() < deadline:
            try:
                result = subprocess.run(
                    [
                        "curl",
                        "-s",
                        "-o",
                        "/dev/null",
                        "-w",
                        "%{http_code}",
                        "-X",
                        "POST",
                        f"http://{endpoint}/v1/chat/completions",
                        "-H",
                        "Content-Type: application/json",
                        "-d",
                        request_body,
                    ],
                    capture_output=True,
                    text=True,
                    timeout=5,
                )
            except subprocess.TimeoutExpired:
                time.sleep(2)
                continue

            status_code = result.stdout.strip()
            if status_code == "200":
                print(f"  Router route for '{mocker_model_name}' is ready")
                return
            if status_code != "404":
                print(f"  Router probe returned {status_code}, retrying...")
            time.sleep(2)

        raise RuntimeError(f"Router route for '{mocker_model_name}' not ready within {timeout}s.")

    def get_router_endpoint(self) -> str:
        """Return a reachable localhost:<port> for the router Service."""
        local_endpoint = f"localhost:{self.local_port}"
        print(
            f"  Starting port-forward ({local_endpoint} → "
            f"svc/{self.ROUTER_DEPLOYMENT}:{self.ROUTER_SVC_PORT})"
        )

        self._pf_proc = subprocess.Popen(
            [
                "kubectl",
                "port-forward",
                f"svc/{self.ROUTER_DEPLOYMENT}",
                f"{self.local_port}:{self.ROUTER_SVC_PORT}",
                "-n",
                self.ROUTER_NAMESPACE,
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )

        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            if self._pf_proc.poll() is not None:
                _, stderr = self._pf_proc.communicate(timeout=5)
                error_msg = stderr.strip() if stderr else f"exit code {self._pf_proc.returncode}"
                raise RuntimeError(f"kubectl port-forward failed: {error_msg}")

            try:
                with socket.create_connection(("localhost", self.local_port), timeout=1):
                    pass
            except OSError:
                time.sleep(0.5)
                continue

            print(f"  Port-forward ready: {local_endpoint}")
            return local_endpoint

        if self._pf_proc.poll() is None:
            stderr = self._read_available_stderr()
            raise RuntimeError(
                f"Port-forward timeout after 15s — port {local_endpoint} not reachable. "
                f"stderr: {stderr if stderr else '<none>'}"
            )

        _, stderr = self._pf_proc.communicate(timeout=5)
        error_msg = stderr.strip() if stderr else "unknown error"
        raise RuntimeError(f"Port-forward exited unexpectedly: {error_msg}")

    def cleanup_port_forward(self) -> None:
        """Stop the port-forward process if one was started."""
        if self._pf_proc is None:
            return
        if self._pf_proc.poll() is None:
            self._pf_proc.terminate()
            try:
                self._pf_proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self._pf_proc.kill()
                self._pf_proc.wait()
        self._pf_proc = None

    def _read_available_stderr(self) -> str:
        if self._pf_proc is None or self._pf_proc.stderr is None:
            return ""
        try:
            ready, _, _ = select.select([self._pf_proc.stderr], [], [], 0.1)
        except (OSError, ValueError):
            return ""
        if not ready:
            return ""
        return self._pf_proc.stderr.read().strip()
