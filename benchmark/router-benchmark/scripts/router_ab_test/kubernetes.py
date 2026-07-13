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
import tempfile
import time
from typing import Any

import requests
import yaml

from router_ab_test.models import BackendProfile, BackendsConfig


class EndpointMode:
    """Endpoint access mode for router service."""

    PORT_FORWARD = "pf"
    LB = "lb"


# ---------------------------------------------------------------------------
# Mocker deployment builder
# ---------------------------------------------------------------------------


class MockerDeploymentBuilder:
    """Build Kubernetes Deployment + Service YAML for mock LLM backends.

    One Deployment is emitted per :class:`BackendProfile`.  All pods share a
    single Service (``selector: {app: mocker-llm}``) so the router sees one
    backend endpoint regardless of how many heterogeneous profiles exist.
    """

    APP_LABEL = "mocker-llm"
    CONTAINER_PORT = 30000

    def __init__(
        self,
        image: str = "ghcr.io/stleox/dynamo-mocker-sglang:v2026-04-23",
        namespace: str = "default",
        cpu_request: str = "500m",
        cpu_limit: str = "2",
        memory_request: str = "512Mi",
        memory_limit: str = "2Gi",
    ):
        self.image = image
        self.namespace = namespace
        self.cpu_request = cpu_request
        self.cpu_limit = cpu_limit
        self.memory_request = memory_request
        self.memory_limit = memory_limit

    def build(self, config: BackendsConfig) -> str:
        """Return a multi-document YAML string ready for ``kubectl apply -f``."""
        docs: list[dict[str, Any]] = []
        for profile in config.profiles:
            docs.append(self._build_deployment(profile, config))
        docs.append(self._build_service())
        return yaml.safe_dump_all(docs, sort_keys=False)

    # ---- Deployment per profile ------------------------------------------------

    def _build_deployment(self, profile: BackendProfile, config: BackendsConfig) -> dict[str, Any]:
        name = f"{self.APP_LABEL}-{profile.name}"
        kv_blocks = profile.kv_cache_blocks if profile.kv_cache_blocks is not None else config.default_kv_cache_blocks
        max_seqs = profile.max_num_seqs if profile.max_num_seqs is not None else config.default_max_num_seqs

        return {
            "apiVersion": "apps/v1",
            "kind": "Deployment",
            "metadata": {
                "name": name,
                "namespace": self.namespace,
                "labels": {
                    "app": self.APP_LABEL,
                    "profile": profile.name,
                },
            },
            "spec": {
                "replicas": profile.count,
                "selector": {
                    "matchLabels": {
                        "app": self.APP_LABEL,
                        "profile": profile.name,
                    },
                },
                "template": {
                    "metadata": {
                        "labels": {
                            "app": self.APP_LABEL,
                            "profile": profile.name,
                        },
                    },
                    "spec": {
                        "containers": [
                            {
                                "name": "mocker",
                                "image": self.image,
                                "imagePullPolicy": "IfNotPresent",
                                "args": [
                                    "--model-path", profile.model,
                                    "--engine-type", profile.engine_type,
                                    "--speedup-ratio", str(profile.speedup_ratio),
                                    "--num-gpu-blocks-override", str(kv_blocks),
                                    "--max-num-seqs", str(max_seqs),
                                ],
                                "ports": [
                                    {
                                        "containerPort": self.CONTAINER_PORT,
                                        "name": "http",
                                        "protocol": "TCP",
                                    },
                                ],
                                "resources": {
                                    "requests": {
                                        "cpu": self.cpu_request,
                                        "memory": self.memory_request,
                                    },
                                    "limits": {
                                        "cpu": self.cpu_limit,
                                        "memory": self.memory_limit,
                                    },
                                },
                                "readinessProbe": {
                                    "httpGet": {
                                        "path": "/v1/models",
                                        "port": self.CONTAINER_PORT,
                                    },
                                    "initialDelaySeconds": 10,
                                    "periodSeconds": 10,
                                },
                                "livenessProbe": {
                                    "httpGet": {
                                        "path": "/v1/models",
                                        "port": self.CONTAINER_PORT,
                                    },
                                    "initialDelaySeconds": 15,
                                    "periodSeconds": 15,
                                },
                            },
                        ],
                    },
                },
            },
        }

    # ---- Shared Service -------------------------------------------------------

    def _build_service(self) -> dict[str, Any]:
        return {
            "apiVersion": "v1",
            "kind": "Service",
            "metadata": {
                "name": self.APP_LABEL,
                "namespace": self.namespace,
                "labels": {"app": self.APP_LABEL},
            },
            "spec": {
                "selector": {"app": self.APP_LABEL},
                "ports": [
                    {
                        "name": "http",
                        "port": self.CONTAINER_PORT,
                        "targetPort": self.CONTAINER_PORT,
                        "protocol": "TCP",
                    },
                ],
                "type": "ClusterIP",
            },
        }


# ---------------------------------------------------------------------------
# Kubernetes resource manager
# ---------------------------------------------------------------------------


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
    _MOCKER_LABEL_SELECTOR = "app=mocker-llm"

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
        self._builder = MockerDeploymentBuilder(namespace=self.MOCKER_NAMESPACE)

    # ---- Mocker backends (built from scenario backends config) -----------------

    def deploy_backends(self, config: BackendsConfig) -> None:
        """Render and apply mocker Deployment(s) + Service from a BackendsConfig."""
        yaml_text = self._builder.build(config)
        with tempfile.NamedTemporaryFile(
            mode="w",
            suffix=".yaml",
            prefix="mocker-backends-",
            delete=False,
        ) as tmp:
            tmp.write(yaml_text)
            manifest_path = tmp.name

        print(f"  Deploying mocker backends from {manifest_path} ...")
        self._apply(manifest_path)
        # Wait for all deployments to roll out
        for profile in config.profiles:
            name = f"{self.MOCKER_DEPLOYMENT}-{profile.name}"
            print(f"  Waiting for deployment/{name} to be ready ...")
            self._wait_for_deployment_ready(name, self.MOCKER_NAMESPACE)

        # Register with Kthena via ModelServer + ModelRoute CRDs
        self._deploy_model_crds(config)

    def _deploy_model_crds(self, config: BackendsConfig) -> None:
        """Apply ModelServer and ModelRoute CRDs for the mocker model."""
        model = config.profiles[0].model

        modelserver = {
            "apiVersion": "networking.serving.volcano.sh/v1alpha1",
            "kind": "ModelServer",
            "metadata": {
                "name": "mocker-model-server",
                "namespace": self.MOCKER_NAMESPACE,
            },
            "spec": {
                "model": model,
                "inferenceEngine": "SGLang",
                "workloadSelector": {"matchLabels": {"app": self.MOCKER_DEPLOYMENT}},
                "workloadPort": {"port": 30000, "protocol": "http"},
            },
        }
        modelroute = {
            "apiVersion": "networking.serving.volcano.sh/v1alpha1",
            "kind": "ModelRoute",
            "metadata": {
                "name": "mocker-model-route",
                "namespace": self.MOCKER_NAMESPACE,
            },
            "spec": {
                "modelName": model,
                "rules": [{"targetModels": [{"modelServerName": modelserver["metadata"]["name"]}]}],
            },
        }

        with tempfile.NamedTemporaryFile(
            mode="w", suffix=".yaml", prefix="model-crds-", delete=False,
        ) as tmp:
            yaml.safe_dump_all([modelserver, modelroute], tmp, sort_keys=False)
            crd_path = tmp.name

        print(f"  Deploying ModelServer + ModelRoute from {crd_path} ...")
        self._apply(crd_path)

    def cleanup_backends(self) -> None:
        """Remove all mocker-llm deployments, the shared Service, and Kthena CRDs."""
        print("  Cleaning up mocker backends ...")
        _run(
            [
                "kubectl", "delete", "deployment",
                "-l", self._MOCKER_LABEL_SELECTOR,
                "-n", self.MOCKER_NAMESPACE,
                "--ignore-not-found",
            ],
        )
        _run(
            [
                "kubectl", "delete", "service",
                self.MOCKER_DEPLOYMENT,
                "-n", self.MOCKER_NAMESPACE,
                "--ignore-not-found",
            ],
        )
        _run(
            [
                "kubectl", "delete", "modelroute",
                "mocker-model-route",
                "-n", self.MOCKER_NAMESPACE,
                "--ignore-not-found",
            ],
        )
        _run(
            [
                "kubectl", "delete", "modelserver",
                "mocker-model-server",
                "-n", self.MOCKER_NAMESPACE,
                "--ignore-not-found",
            ],
        )

    # ---- Router config --------------------------------------------------------

    def apply_router_config(self, config_path: str) -> None:
        print(f"  Applying router config: {config_path}")
        self._apply(config_path)

        print(f"  Restarting router deployment {self.ROUTER_DEPLOYMENT}...")
        _run(
            [
                "kubectl", "rollout", "restart",
                f"deployment/{self.ROUTER_DEPLOYMENT}",
                "-n", self.ROUTER_NAMESPACE,
            ],
        )
        self._wait_for_deployment_ready(self.ROUTER_DEPLOYMENT, self.ROUTER_NAMESPACE, timeout=120)

    def wait_for_router_ready(self, mocker_model_name: str, endpoint: str, timeout: int = 300) -> None:
        """Probe the router to confirm the route table has the given model loaded."""
        print(f"  Probing router for model={mocker_model_name} at {endpoint}...")
        deadline = time.monotonic() + timeout
        url = f"http://{endpoint}/v1/chat/completions"
        payload = {
            "model": mocker_model_name,
            "messages": [{"role": "user", "content": "ping"}],
            "max_tokens": 1,
        }
        while time.monotonic() < deadline:
            try:
                resp = requests.post(url, json=payload, timeout=5)
            except (requests.Timeout, requests.ConnectionError):
                time.sleep(5)
                continue

            if resp.status_code == 200:
                print(f"  Router route for '{mocker_model_name}' is ready")
                return
            body_preview = resp.text.strip()[:200]
            print(f"  Router probe returned {resp.status_code}: {body_preview}")
            time.sleep(5)

        raise RuntimeError(f"Router route for '{mocker_model_name}' not ready within {timeout}s.")

    # ---- Endpoints -----------------------------------------------------------

    def get_router_endpoint(self) -> str:
        if self.endpoint_mode == EndpointMode.LB:
            return self._get_lb_endpoint()
        return self._start_port_forward(
            process_attr="_pf_proc",
            local_port=self.local_port,
            remote_port=self.ROUTER_SVC_PORT,
            description=f"svc/{self.ROUTER_SVC_NAME}:{self.ROUTER_SVC_PORT}",
        )

    def _get_lb_endpoint(self) -> str:
        result = subprocess.run(
            [
                "kubectl", "get", "svc", self.ROUTER_SVC_NAME,
                "-n", self.ROUTER_NAMESPACE,
                "-o", "jsonpath={.status.loadBalancer.ingress[0].ip}",
            ],
            capture_output=True,
            text=True,
        )
        external_ip = result.stdout.strip()
        if not external_ip:
            raise RuntimeError(
                f"LoadBalancer Service {self.ROUTER_SVC_NAME} has no EXTERNAL-IP."
            )

        result = subprocess.run(
            [
                "kubectl", "get", "svc", self.ROUTER_SVC_NAME,
                "-n", self.ROUTER_NAMESPACE,
                "-o", "jsonpath={.spec.ports[0].port}",
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
        return self._start_port_forward(
            process_attr="_debug_pf_proc",
            local_port=self.debug_local_port,
            remote_port=self.ROUTER_DEBUG_PORT,
            description=f"deployment/{self.ROUTER_DEPLOYMENT}:{self.ROUTER_DEBUG_PORT}",
            target_type="deployment",
        )

    # ---- Port-forward lifecycle -----------------------------------------------

    def cleanup_port_forward(self) -> None:
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
                "kubectl", "port-forward", target,
                f"{local_port}:{remote_port}",
                "-n", self.ROUTER_NAMESPACE,
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

    # ---- Internal helpers -----------------------------------------------------

    def _apply(self, manifest_path: str) -> None:
        _run(["kubectl", "apply", "-f", manifest_path])
        time.sleep(5)

    def _wait_for_deployment_ready(self, name: str, namespace: str, timeout: int = 120) -> None:
        _run(
            [
                "kubectl", "rollout", "status",
                f"deployment/{name}",
                "-n", namespace,
                f"--timeout={timeout}s",
            ],
        )

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


# ---------------------------------------------------------------------------
# Module-level helper
# ---------------------------------------------------------------------------


def _run(cmd: list[str]) -> None:
    subprocess.run(cmd, check=True, capture_output=True, text=True)
