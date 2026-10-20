# Build an Endpoint Discovery Provider

This example compiles an HTTP/JSON provider into a custom standalone Router.
It uses the existing file loader for ModelServer/ModelRoute configuration,
and HTTP snapshots for instance membership. Kubernetes, a GPU and Redis are
not required.

## Implement and register

The complete source is in
[examples/kthena-router/endpoint-discovery](https://github.com/volcano-sh/kthena/tree/main/examples/kthena-router/endpoint-discovery):

| File | Responsibility |
| --- | --- |
| `http.go` | Validate the `url` parameter; fetch a full JSON array using the Snapshot context |
| `main.go` | Register `http-json` in `Server.EndpointDiscoveryRegistry` before `Run` |
| `router.yaml` | Select the provider and map `default/demo` to its registry URL |
| `resources/model.yaml` | Declare ModelServer `demo` and the request model route |
| `demo/main.go`, `endpoints.json` | Local registry and mock inference backend |

The registry returns existing API `Endpoint` objects:

```json
[{"name":"instance-a","address":"127.0.0.1","port":18081}]
```

`Snapshot` returns an error for non-200 responses, invalid JSON, `null` or bodies
over 1 MiB. Only a valid array, including `[]`, is a successful snapshot. The
controller validates endpoints and reconciles Store; the provider does not
construct Pods or modify Store. This polling example owns no background worker
or connection pool, so `Close` is a no-op. A watch-based provider can call the
factory's `notify` callback and must release its subscription in `Close`.

## Build and run

Use the Go version from `go.mod`, and run all commands from the repository root.
The default Router binary does **not** register this provider.

```bash
go build -o bin/kthena-http-router ./examples/kthena-router/endpoint-discovery
go build -o bin/kthena-discovery-demo ./examples/kthena-router/endpoint-discovery/demo
```

In terminal 1, start the fixture:

```bash
bin/kthena-discovery-demo
```

In terminal 2, start the custom Router:

```bash
bin/kthena-http-router
```

The example uses ports 18080 (Router), 18081 (fixture), 19090 (metrics) and
15000 (debug); keep these free. Router settings and resource paths default to
the files in the example directory. This local smoke test has no authentication;
run it only in a trusted development environment.

In terminal 3, wait for `/readyz` to return HTTP 200, then send a non-streaming
request:

```bash
curl --fail http://127.0.0.1:18080/readyz
curl --fail http://127.0.0.1:18080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"demo","messages":[{"role":"user","content":"Hello"}],"stream":false}'
```

The response contains `Hello from the discovered endpoint`. The fixture returns
canned content and Usage; this verifies discovery and forwarding, not inference.

## Verify refresh

Edit `endpoints.json` without restarting either process. Save `[]` to remove the
instance, then restore the original array to add it back. Observe
`kthena_router_discovery_endpoints{model_server="default/demo"}` change from 1 to
0 and back after background refreshes:

```bash
curl --silent http://127.0.0.1:19090/metrics | grep kthena_router_discovery_endpoints
```

With an empty pool, inference fails but `/readyz` stays ready after its startup
gate has latched. Invalid JSON or an unavailable registry instead retains the
last successful membership. Stop both processes with Ctrl-C.

For a real integration, replace the fixture URL and supply routable endpoints;
add registry authentication/transport settings as needed. See
[configuration and ownership](./config-router.md#configuration-and-ownership)
for identity rules, timeouts and restart requirements.
