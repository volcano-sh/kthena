# AGENTS Guidelines for Kthena

Kthena is a Kubernetes-native LLM inference platform (controllers, router, CLI, Helm charts).
Human-oriented docs live in [CONTRIBUTING.md](./CONTRIBUTING.md) and [docs/kthena/docs/](docs/kthena/docs/).
This file gives coding agents the minimum context to work safely in the whole repository.

## Project layout

| Path | Role |
|------|------|
| `pkg/` | Core business logic (controllers, router, APIs, webhooks) |
| `cmd/` | Binary entrypoints (`kthena-controller-manager`, `kthena-router`) |
| `cli/` | `kthena` CLI and kubectl plugin |
| `client-go/` | Generated Kubernetes clients (do not hand-edit) |
| `charts/` | Helm charts and embedded CRDs |
| `docs/kthena/` | Published documentation (Docusaurus) |
| `examples/` | Sample YAML for CRDs and deployments |
| `test/e2e/` | Kind-based end-to-end tests |
| `hack/` | Codegen scripts and boilerplate |
| `docker/` | Dockerfiles for Go and Python images |
| `python/` | Python runtime/downloader components |
| `benchmark/` | Router performance benchmarks |

## Dev environment

- **Go**: version in [`go.mod`](./go.mod). See [development-setup.md](docs/kthena/docs/developer-guide/development-setup.md).
- **Kubernetes**: 1.28+ with CRD support for development; E2E uses Kind (see [test/e2e/README.md](test/e2e/README.md)).
- **Module path**: `github.com/volcano-sh/kthena`.
- Read linked docs below before large changes.

## Build and verification commands

Run from the repository root:

| Command | Purpose |
|---------|---------|
| `make lint` | `golangci-lint` on Go code |
| `make test` | Unit tests (excludes `test/e2e` and `client-go`; runs `generate` first) |
| `make build` | Build `bin/kthena-router`, `bin/kthena-controller-manager`, `bin/kthena` |
| `make generate` | Regenerate CRDs, deepcopy, client-go, and docs artifacts |
| `make gen-check` | Fail if `make generate` would dirty the tree |
| `make fmt` | `go fmt ./...` |
| `make lint-python` | Ruff on `python/` |
| `make test-docs` | Docusaurus typecheck and build under `docs/kthena/` |

Scoped examples:

```bash
go test ./pkg/kthena-router/...
go test -race ./pkg/autoscaler/...
```

E2E (requires Kind, Docker, Helm): `make test-e2e-controller-manager`, `make test-e2e-router`, `make test-e2e-gateway-api`, `make test-e2e-gateway-inference-extension`. See [test/e2e/README.md](test/e2e/README.md) for categories and CI matrix. Tear down with `make test-e2e-cleanup`.

## Coding conventions

- **Make no mistakes** and follow existing codebase patterns while writing code.
- Follow [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments); use `gofmt` / `make fmt`.
- Keep **user-facing APIs backward compatible**: CRDs, HTTP router APIs, CLI flags.
- Put implementation in `pkg/`, not `cmd/` (thin `main` only).
- After API or CRD field changes, run `make generate` and commit all generated files.
- Use table-driven unit tests; add `-race` for concurrency-sensitive code.
- Major design changes belong in `docs/proposal/` and should be discussed before large PRs.

## Testing instructions

- Add or update `*_test.go` for every behavior change in Go packages.
- Run `make lint` and `make test` before opening a PR (`make gen-check` if APIs or generated files changed).
- For cluster-level behavior, use or extend `test/e2e/`; follow [test/e2e/README.md](test/e2e/README.md).
- E2E: use helpers under `test/e2e/framework/` and `test/e2e/utils/`; install via the same Helm path as `test/e2e/setup.sh`.
- When E2E changes, run the matching `make test-e2e-*` target locally; describe the category in the PR.
- Do not change tests solely to pass CI (weaker assertions, arbitrary sleeps, inflated timeouts/retries, skips, or unrelated conditionals). Fix the bug or use proper wait/poll helpers.

## Documentation

- User and developer docs: [docs/kthena/docs/](docs/kthena/docs/) (edit here, not only versioned snapshots unless releasing).
- Architecture: [docs/kthena/docs/architecture/](docs/kthena/docs/architecture/).
- CRD reference: generated via `make gen-docs` / `make generate`; see [workload CRDs](docs/kthena/docs/reference/crd/workload.serving.volcano.sh.md) and [networking CRDs](docs/kthena/docs/reference/crd/networking.serving.volcano.sh.md).
- Update `examples/` when CRD shapes or deployment flows change.

## Pull request instructions

- Target branch: `main`. Use focused PRs; avoid unrelated drive-by refactors.
- Fill the PR template: problem, summary, testing, linked issues.
- Request review from area owners listed in the nearest `OWNERS` file.
- Significant features: note `release-note` in the PR when required.
- Disclose AI assistance in the PR template’s reviewer notes when applicable (see below).

## AI-assisted contributions

Aligned with [CONTRIBUTING.md](./CONTRIBUTING.md#ai-guidance):

- You are responsible for understanding every line you submit; review and test before opening a PR.
- Prefer small, reviewable changes over large generated dumps.
- Do not use AI to draft review replies; maintainers expect human responses.
- Disclose AI tool use in the PR when asked.
- Follow **Testing instructions** above before submitting; do not rely on reviewers to catch missing verification.

## Security

Report sensitive issues to volcano-security@googlegroups.com instead of public issues.

## Further reading

- [CONTRIBUTING.md](./CONTRIBUTING.md)
- [Architecture overview](docs/kthena/docs/architecture/architecture.mdx)
- [Development setup](docs/kthena/docs/developer-guide/development-setup.md)
- [CI](docs/kthena/docs/developer-guide/ci.md)
- [E2E tests](test/e2e/README.md)
- [Documentation index](docs/kthena/docs/intro.md)
