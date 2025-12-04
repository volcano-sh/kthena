# Contributing to Kthena

Thank you for your interest in improving Kthena! This guide outlines how to get started, the workflow we follow, and the expectations for contributors. The structure closely follows the [Volcano](https://github.com/volcano-sh/volcano/blob/master/contribute.md) contributor guide, with adjustments for the Kthena project.

## 👋 How to Get Involved

* Join the conversation on GitHub:
  * [Issues](https://github.com/volcano-sh/kthena/issues) for bug reports and feature requests
  * [Discussions](https://github.com/volcano-sh/kthena/discussions) for design questions and ideas
* Chat with maintainers via Slack (link in the project README)
* Review the [code of conduct](https://github.com/volcano-sh/community/blob/master/code_of_conduct.md) before participating

## 🚀 Contribution Workflow

### 1. Pick or Propose Work

* Search open issues labeled `good first issue` or `help wanted`
* If you have a new idea, open an issue describing the motivation, proposal, and alternatives
* For large changes, propose an enhancement in Discussions first to get feedback

### 2. Set Up Your Environment

* Install Go (version listed in `go.mod`)
* Clone the repository and ensure scripts in `Makefile` run locally:
  * `make lint`
  * `make test`
  * `make build`

### 3. Create a Working Branch

```bash
# from the repo root
git checkout main
git pull origin main
git checkout -b feat/<short-description>
```

### 4. Develop with Tests

* Follow Go best practices and respect existing module structure under `pkg/` and `cmd/`
* Maintain backwards compatibility for user-facing APIs (CRDs, CLI, HTTP schemas)
* Add unit tests in the relevant `*_test.go` files or integration tests under `test/`
* Run `make lint` and `make test` before submitting your changes

### 5. Commit Conventions

* Use clear, descriptive commit messages: `component: summary`
* Reference issues with `Fixes #<issue-number>` or `Refs #<issue-number>` when applicable
* Sign off your commits if required by your employer/company policies

### 6. Keep Your Branch Up to Date

* Rebase frequently on `main` to reduce merge conflicts:

```bash
git fetch origin
git rebase origin/main
```

* Resolve conflicts locally and rerun tests after rebasing

### 7. Open a Pull Request

* Push your branch and create a PR against `main`
* Fill in the PR template, covering:
  * Problem statement
  * Summary of changes
  * Testing performed
  * Screenshots/logs if relevant
* Link related issues or discussions for context
* Request review from maintainers or area owners (see the `OWNERS` directories)

### 8. Code Review

To make it easier for your PR to receive reviews, consider the reviewers will need you to:

* follow [good coding guidelines](https://go.dev/wiki/CodeReviewComments).
* write [good commit messages](https://chris.beams.io/posts/git-commit/).
* break large changes into a logical series of smaller patches which individually make easily understandable changes, and in aggregate solve a broader issue.
* label PRs with appropriate reviewers: to do this read the messages the bot sends you to guide you through the PR process.

### 9. Address Review Feedback

* Be responsive to comments and iterate quickly
* Reply to each comment once addressed
* Squash commits if asked by reviewers to keep history clean

### 10. Celebrate the Merge 🎉

* Once the PR is approved and checks are green, the maintainer will merge it
* Your contribution becomes part of the Kthena history!

## 📐 Coding Standards

* Follow Go `gofmt` formatting automatically (run `go fmt ./...`)
* Maintain consistent log semantics via the shared logging packages under `pkg/`
* Keep public API changes backward compatible; update CRDs and generated clients when fields change (`make generate`)
* Document new features under `docs/` and update READMEs/examples when behavior changes

## 🧪 Testing Guidelines

* Unit tests are mandatory for new functionality and bug fixes
* Use table-driven tests where appropriate for clarity
* For concurrency-sensitive code, add race detector checks (`go test -race ./...`)
* Integration tests should target scenarios under `test/` or `examples/`

## 📚 Documentation Expectations

* Update relevant docs under `docs/kthena/docs/`
* Provide getting-started examples if introducing new CRDs or CLI commands
* Refresh charts/examples under `examples/` when changing deployments
* Include release notes summary for significant changes (tag `release-note`) in PRs

## 🧭 Governance and Ownership

* Maintainers are listed in the top-level [`OWNERS`](./OWNERS) file and `OWNERS` files in subdirectories
* Subsystem owners review and approve changes in their areas
* Major design decisions go through design docs in `docs/proposal/`

## 🤖 CI/CD and Status Checks

Kthena uses GitHub Actions for continuous integration. Several workflows use path filters to optimize CI runs:

* `go-tests.yml` - Runs only when Go code changes
* `python-tests.yml` - Runs only when Python code changes
* `docs-tests.yml` - Runs only when documentation changes
* `e2e-tests.yml` - Runs when relevant code or configuration changes

### Status Check Pattern

Each workflow includes a `status-check` job that acts as a reliable status gate for branch protection rules. This job:

* Always runs (even when the main job is skipped due to path filters)
* Passes if the main job succeeds OR is skipped
* Fails only if the main job actually fails

This ensures that required status checks don't block PRs when workflows are legitimately skipped due to path filters.

**For Contributors**: When adding new workflows with path filters, include a similar status-check job to ensure they work correctly with branch protection rules.

## 🛠 Tooling

* `make lint` runs linters (`golangci-lint`)
* `make test` runs unit tests
* `make build` produces controller binaries under `bin/`
* `make generate` regenerates codegen artifacts (CRDs, clients)
* `make gen-docs` builds documentation (Docusaurus)

## 🔐 Security Reporting

* For sensitive issues, email volcano-security@googlegroups.com instead of filing a public issue
* Provide steps to reproduce, affected components, and impact assessment

## 📝 License

By contributing, you agree that your contributions will be licensed under the Apache 2.0 License. See [LICENSE](./LICENSE) for details.
