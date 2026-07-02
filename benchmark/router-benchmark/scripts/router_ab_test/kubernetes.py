# Copyright The Volcano Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
from __future__ import annotations

import select
import socket
import subprocess
import time


class EndpointMode:
    """Endpoint access mode for router service."""

    PORT_FORWARD = "pf"
    LB = "lb"


class K8sManager:
    """Manage Kubernetes resources needed by the benchmark."""

    ROUTER_NAMESPACE = "kthena-system"
    ROUTER_DEPLOYMENT = "kthena-router"  # SVC uses the same name
    ROUTER_SVC_PORT = 80
    ROUTER_SVC_NAME = "kthena-router"
    ROUTER_DEBUG_PORT = 15000
    DEFAULT_LOCAL_PORT = 8080
    DEFAULT_DEBUG_LOCAL_PORT = 18080
    MOCKER_NAMESPACE = "default"
    MOCKER_DEPLOYMENT = "mocker-llm"

    def __init__(
            self,
            namespace: str = "default",
            local_port: int = DEFAULT_LOCAL_PORT,
            debug_local_port: int = DEFAULT_DEBUG_LOCAL_PORT,
            endpoint_mode: str = EndpointMode.PORT_FORWARD,
    ):
        self.namespace = namespace
        self.local_port = local_port
        self.debug_local_port = debug_local_port
        self.endpoint_mode = endpoint_mode
        self._pf_proc: subprocess.Popen[str] | None = None
        self._debug_pf_proc: subprocess.Popen[str] | None = None

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
                "-n",
                self.MOCKER_NAMESPACE,
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
        """Return a reachable endpoint for the router Service.

        In port-forward mode, returns localhost:<port> via kubectl port-forward.
        In lb mode, returns <external_ip>:<port> from LoadBalancer Service status.
        """
        if self.endpoint_mode == EndpointMode.LB:
            return self._get_lb_endpoint()
        return self._start_port_forward(
            process_attr="_pf_proc",
            local_port=self.local_port,
            remote_port=self.ROUTER_SVC_PORT,
            description=f"svc/{self.ROUTER_SVC_NAME}:{self.ROUTER_SVC_PORT}",
        )

    def _get_lb_endpoint(self) -> str:
        """Get router endpoint from LoadBalancer Service EXTERNAL-IP.

        For multi-node clusters where the router Service type is LoadBalancer.
        Returns <external_ip>:<service_port>.
        """
        result = subprocess.run(
            [
                "kubectl",
                "get",
                "svc",
                self.ROUTER_SVC_NAME,
                "-n",
                self.ROUTER_NAMESPACE,
                "-o",
                "jsonpath={.status.loadBalancer.ingress[0].ip}",
            ],
            capture_output=True,
            text=True,
        )
        external_ip = result.stdout.strip()

        if not external_ip:
            raise RuntimeError(
                f"LoadBalancer Service {self.ROUTER_SVC_NAME} has no EXTERNAL-IP. "
            )

        # Get the service port from the Service
        result = subprocess.run(
            [
                "kubectl",
                "get",
                "svc",
                self.ROUTER_SVC_NAME,
                "-n",
                self.ROUTER_NAMESPACE,
                "-o",
                "jsonpath={.spec.ports[0].port}",
            ],
            capture_output=True,
            text=True,
        )
        port = result.stdout.strip()
        if not port:
            raise RuntimeError("Failed to get port from router Service")

        endpoint = f"{external_ip}:{port}"
        print(f"  Using LB endpoint: {endpoint}")
        return endpoint

    def get_router_debug_endpoint(self) -> str:
        """Return a reachable localhost:<port> for the router debug server."""
        return self._start_port_forward(
            process_attr="_debug_pf_proc",
            local_port=self.debug_local_port,
            remote_port=self.ROUTER_DEBUG_PORT,
            description=f"deployment/{self.ROUTER_DEPLOYMENT}:{self.ROUTER_DEBUG_PORT}",
            target_type="deployment",
        )

    def cleanup_port_forward(self) -> None:
        """Stop all port-forward processes started by the benchmark.

        In port-forward mode, stops both main and debug port-forward.
        In lb mode, only debug port-forward needs cleanup.
        """
        if self.endpoint_mode == EndpointMode.PORT_FORWARD:
            self._stop_port_forward("_pf_proc")
        self._stop_port_forward("_debug_pf_proc")

    def _start_port_forward(
            self,
            process_attr: str,
            local_port: int,
            remote_port: int,
            description: str,
            target_type: str = "svc",
    ) -> str:
        existing_proc = getattr(self, process_attr)
        if existing_proc is not None and existing_proc.poll() is None:
            return f"localhost:{local_port}"

        local_endpoint = f"localhost:{local_port}"
        print(f"  Starting port-forward ({local_endpoint} → {description})")

        target = f"{target_type}/{self.ROUTER_DEPLOYMENT}"
        proc = subprocess.Popen(
            [
                "kubectl",
                "port-forward",
                target,
                f"{local_port}:{remote_port}",
                "-n",
                self.ROUTER_NAMESPACE,
            ],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            text=True,
        )
        setattr(self, process_attr, proc)

        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            if proc.poll() is not None:
                _, stderr = proc.communicate(timeout=5)
                error_msg = stderr.strip() if stderr else f"exit code {proc.returncode}"
                raise RuntimeError(f"kubectl port-forward failed: {error_msg}")

            try:
                with socket.create_connection(("localhost", local_port), timeout=1):
                    pass
            except OSError:
                time.sleep(0.5)
                continue

            print(f"  Port-forward ready: {local_endpoint}")
            return local_endpoint

        if proc.poll() is None:
            stderr = self._read_available_stderr(proc)
            raise RuntimeError(
                f"Port-forward timeout after 15s — port {local_endpoint} not reachable. "
                f"stderr: {stderr if stderr else '<none>'}"
            )

        _, stderr = proc.communicate(timeout=5)
        error_msg = stderr.strip() if stderr else "unknown error"
        raise RuntimeError(f"Port-forward exited unexpectedly: {error_msg}")

    def _stop_port_forward(self, process_attr: str) -> None:
        proc = getattr(self, process_attr)
        if proc is None:
            return
        if proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
        setattr(self, process_attr, None)

    @staticmethod
    def _read_available_stderr(proc: subprocess.Popen[str]) -> str:
        if proc.stderr is None:
            return ""
        try:
            ready, _, _ = select.select([proc.stderr], [], [], 0.1)
        except (OSError, ValueError):
            return ""
        if not ready:
            return ""
        return proc.stderr.read().strip()
