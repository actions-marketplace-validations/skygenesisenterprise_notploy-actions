# Notploy Deployment — GitHub Action

[![License: MIT](https://img.shields.io/badge/License-MIT-purple.svg)](./LICENSE)
[![Go 1.24](https://img.shields.io/badge/Go-1.24-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![GitHub Actions: container action](https://img.shields.io/badge/GitHub%20Actions-container%20action-blue?logo=githubactions)](./action.yml)
[![CI](https://github.com/skygenesisenterprise/notploy-actions/actions/workflows/ci.yml/badge.svg)](https://github.com/skygenesisenterprise/notploy-actions/actions/workflows/ci.yml)
[![PRs welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](./.github/CONTRIBUTING.md)
[![Discord](https://img.shields.io/badge/Discord-join-5865F2?logo=discord&logoColor=white)](https://discord.gg/GUkDugVh34)

> Deploy an application to Notploy from GitHub Actions, wait for the result, and report it back to the run.

**Notploy Actions** is the official GitHub Action for [Notploy](https://github.com/skygenesisenterprise/notploy). It triggers a deployment on your Notploy instance, follows it until it reaches a final state, and surfaces the outcome where it matters: step outputs, a job summary, the deployment logs on failure, and an optional commit status on the checked-out revision.

It is a Go program packaged as a container action: no Node.js, no pnpm, no runtime dependencies — and the Go module itself uses only the standard library.

**Perfect for:** teams that already run Notploy and want continuous deployment from GitHub, platform teams standardising deployments across repositories, and anyone who wants a failed deployment to fail the workflow — with the logs to prove why.

## Contents

- [Notploy Deployment — GitHub Action](#notploy-deployment--github-action)
  - [Contents](#contents)
  - [Why this action matters](#why-this-action-matters)
  - [What you can do](#what-you-can-do)
    - [Deploy](#deploy)
    - [Observe](#observe)
    - [Automate](#automate)
  - [Quick start](#quick-start)
  - [Configuration](#configuration)
    - [API modes](#api-modes)
    - [Inputs](#inputs)
    - [Outputs](#outputs)
    - [Behaviour on failure](#behaviour-on-failure)
  - [Examples](#examples)
  - [How the action is built](#how-the-action-is-built)
  - [Development](#development)
    - [Root commands](#root-commands)
    - [Running the action outside a runner](#running-the-action-outside-a-runner)
    - [Tests](#tests)
  - [FAQ](#faq)
    - [Is it free?](#is-it-free)
    - [Which Notploy API does it use?](#which-notploy-api-does-it-use)
    - [Why a container action?](#why-a-container-action)
    - [Does it use GITHUB\_TOKEN?](#does-it-use-github_token)
    - [How do I get help or report a bug?](#how-do-i-get-help-or-report-a-bug)
  - [Related resources](#related-resources)
    - [Official](#official)
    - [Community](#community)
    - [Commercial](#commercial)
  - [Contributing](#contributing)
  - [Security](#security)
  - [License](#license)
  - [Note](#note)

## Why this action matters

🚀 **Deployments that block the pipeline** — With `wait: true` (the default), a deployment that fails in Notploy fails the GitHub job, so a broken release never goes unnoticed.

🔁 **Exactly one deployment per run** — The CI/CD API accepts an idempotency key that defaults to the run id and attempt: retries replay the original deployment instead of queueing a second one.

🔌 **Works with every instance** — `api-mode: auto` prefers the modern CI/CD API and falls back to the classic endpoints when the instance or the credential predates it, without silently degrading on real errors.

📋 **The outcome, where you look** — Step outputs, a structured job summary, the tail of the deployment log on failure (escaped so it cannot inject workflow commands), and an optional commit status on the sha you deployed.

🧩 **Zero dependencies, auditable** — A single static Go binary built from the standard library alone; the whole trust path from `action.yml` to the HTTP request is this repository.

## What you can do

### Deploy

- Trigger a deployment of any Notploy application (id, `appName` or dashboard name) on push, on pull request, on a tag, or manually.
- Scope the deployment to a project and an environment on the CI/CD API.
- Build a container image in GitHub Actions, push it to a registry, then ask Notploy to deploy that exact tag.
- Cancel the deployment in Notploy when the wait gives up or the job is cancelled (`cancel-on-timeout`).

### Observe

- Wait for the deployment to reach `done`, `error` or `cancelled`, with a configurable timeout and poll interval.
- Read the last `log-tail` lines of a failed or cancelled deployment straight in the workflow log and the job summary.
- Publish the deployment as a commit status (`notploy/<application>` by default) so the revision itself shows the deploy state.
- Link onward to the Notploy dashboard, the public application URL and the log endpoint through outputs.

### Automate

- Drive everything from workflow inputs: pick the environment with `workflow_dispatch`, feed outputs into later jobs or smoke tests.
- Send the full GitHub run context (repository, commit, ref, workflow, run) to Notploy so the deployment links back to the run that created it.
- Run the binary locally with the same inputs as environment variables, outside any runner.

## Quick start

Add the action to a job and give it three inputs — the endpoint, a credential and the application to deploy:

```yaml
name: Deploy

on:
  push:
    branches: [main]

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6

      - name: Deploy to Notploy
        uses: skygenesisenterprise/notploy-actions@v1
        with:
          endpoint: ${{ secrets.NOTPLOY_URL }}
          api-key: ${{ secrets.NOTPLOY_API_KEY }}
          application-id: ${{ secrets.NOTPLOY_APPLICATION_ID }}
```

Store `NOTPLOY_URL`, `NOTPLOY_API_KEY` and `NOTPLOY_APPLICATION_ID` as repository or environment secrets. A [CI/CD token](https://docs.notploy.com) is the recommended credential; a classic API key also works through the legacy endpoints. `wait` defaults to `true`, so this step fails when the deployment fails.

## Configuration

### API modes

| `api-mode` | Behaviour |
| --- | --- |
| `auto` (default) | Prefer the CI/CD API; fall back to the classic endpoints **only** when the instance or the credential has no CI/CD API (401/403 CI/CD messages, or a 404 route miss) — and always **before** any deployment is created. |
| `cicd` | Require the CI/CD API. No fallback: an older instance fails with an actionable hint instead of silently degrading. |
| `legacy` | Only the classic endpoints (`application.deploy` + `deployment.all`), for pinned setups. Also forced by `metadata: false`. |

Fallback never happens on validation errors (400), conflicts (409), rate limits (429) or server errors (5xx), and never after a deployment was triggered. With `metadata: false` the GitHub run context is not sent to Notploy, which the CI/CD API requires — so the mode becomes `legacy` automatically (`api-mode: cicd` + `metadata: false` is rejected as a configuration error).

### Inputs

| Input | Required | Default | Description |
| --- | --- | --- | --- |
| `endpoint` | yes | — | Base URL of the Notploy instance, e.g. `https://notploy.example.com`. Do **not** append `/api`. |
| `api-key` | yes | — | Notploy credential, sent as `x-api-key`. Always a secret. |
| `application-id` | yes | — | Application to deploy: id, `appName` or dashboard name. |
| `api-mode` | no | `auto` | `auto`, `cicd` or `legacy` — see [API modes](#api-modes). |
| `project` | no | — | Project id or name scoping the CI/CD request. Ignored by the legacy endpoints. |
| `environment` | no | — | Environment id or name scoping the CI/CD request. Ignored by the legacy endpoints. |
| `title` | no | workflow + run + commit | Deployment title (max 200 chars). |
| `description` | no | event, ref, actor, run | Deployment description (max 2000 chars). |
| `metadata` | no | `true` | Send the GitHub run context to Notploy (links the deployment back to the run). `false` ⇒ legacy mode. |
| `idempotency-key` | no | `gha-<run id>-<run attempt>` | Makes this run equal exactly one deployment on the CI/CD API: retries replay the original deployment instead of queueing a second one. Legacy endpoints are never retried. |
| `wait` | no | `true` | Wait for a final state. `false` returns once accepted, status `triggered`. |
| `timeout` | no | `30m` | Maximum wait (Go duration: `30m`, `90s`, `1h`). |
| `poll-interval` | no | `5s` | Delay between status lookups (min `1s`). |
| `cancel-on-timeout` | no | `false` | Cancel the deployment in Notploy when the wait gives up or the job is cancelled. |
| `log-tail` | no | `100` | Log lines read from a failed or cancelled deployment (`0` disables, max `5000`). |
| `github-token` | no | — | Token publishing a commit status (`statuses: write`). Never the `api-key`. |
| `github-status-context` | no | `notploy/<application>` | Commit status context; set it when several jobs deploy different applications. |

Durations use Go syntax (`s`, `m`, `h`); days (`1d`) are not supported.

### Outputs

| Output | Description |
| --- | --- |
| `deployment-id` | Identifier of the Notploy deployment. Empty when the legacy endpoints could not identify this run's deployment, or when it was only triggered. |
| `deployment-status` | `queued`, `running`, `done`, `error`, `cancelled`, or `triggered` when `wait: false`. |
| `deployment-url` | Public URL of the application, when it has an enabled domain. Notploy has no per-deployment URL. |
| `deployment-details-url` | Link to the application's page in the Notploy dashboard (its deployment logs). |
| `deployment-logs-url` | API endpoint returning this deployment's log (authenticated; the job summary is the human-readable alternative). |
| `deployment-environment` | Environment name as reported by Notploy, or the `environment` input when the surface does not report it. |
| `deployment-idempotent` | `true` when the CI/CD API replayed an existing deployment, `false` when it created one, empty in legacy mode. |

Outputs are written even when the step fails, so later steps can still link to the failed deployment.

### Behaviour on failure

| Situation | Step result |
| --- | --- |
| Deployment reaches `done` | success |
| Deployment reaches `error` | **failure** — with the id, status, `errorMessage` and the last `log-tail` lines |
| Deployment reaches `cancelled` | success with a warning (a cancelled deployment is not a CI failure) |
| `timeout` reached while unfinished | **failure**, after reporting the last observed status |
| Job cancelled (SIGTERM) | cancelled; the deployment is also cancelled when `cancel-on-timeout: true` |
| Bad request, unknown application, forbidden (400/403/404) | **failure**, with an actionable hint about which input to check |
| Instance or credential without a CI/CD API (and `api-mode: auto`) | silent fallback to the legacy endpoints, logged once |
| Temporary network / 5xx / 429 errors | retried (the trigger only when an idempotency key applies), then the step fails |

## Examples

Ready-to-copy workflows live in [`examples/`](./examples) and are linted by `actionlint` in CI:

| File | Shows |
| --- | --- |
| [`examples/basic.yml`](./examples/basic.yml) | Deploy on push to `main` |
| [`examples/docker-build.yml`](./examples/docker-build.yml) | Build and push an image, deploy that exact tag, smoke test |
| [`examples/pull-request.yml`](./examples/pull-request.yml) | Deploy a preview per pull request |
| [`examples/manual-dispatch.yml`](./examples/manual-dispatch.yml) | `workflow_dispatch` with a choice of environments |
| [`examples/multiple-environments.yml`](./examples/multiple-environments.yml) | Staging then production with different secrets |
| [`examples/commit-status.yml`](./examples/commit-status.yml) | Publish the deployment as a commit status |
| [`examples/no-wait.yml`](./examples/no-wait.yml) | Fire and forget, using the outputs in a later job |
| [`examples/cancel-on-timeout.yml`](./examples/cancel-on-timeout.yml) | Bound the wait and cancel the deployment in Notploy |

## How the action is built

```
notploy-actions/
├── action.yml                     # Action metadata: inputs, outputs, Docker entrypoint
├── Dockerfile                     # Multi-stage build of the Go binary
├── cmd/notploy-action/            # Entry point: wiring, outputs, summary, exit code
└── internal/
    ├── config/                    # Inputs → validated configuration
    ├── github/                    # Everything GitHub-specific: context, inputs,
    │                              # outputs, summary, commit statuses
    ├── notploy/                   # HTTP client for both API surfaces
    ├── deployment/                # Lifecycle: trigger, correlate, poll, settle
    └── version/                   # Version reported in the User-Agent
```

| Path | Role |
| --- | --- |
| `cmd/notploy-action` | Loads the configuration, masks the credentials, wires the client and the lifecycle, publishes outputs, the summary and the commit status, chooses the exit code |
| `internal/config` | Turns `INPUT_*` values into a validated configuration with explicit defaults and bounds |
| `internal/github` | The only package allowed to read `GITHUB_*` / `INPUT_*`: run context, outputs, job summary, commit statuses |
| `internal/notploy` | HTTP client for the CI/CD surface (`cicd.go`) and the classic endpoints (`client.go`), with retry and actionable errors |
| `internal/deployment` | The lifecycle itself — trigger, correlate, poll, settle, collect logs — free of any GitHub-specific code |

The layering is deliberate: `internal/notploy` and `internal/deployment` take parameters and a logger, so the lifecycle is unit-testable without a runner, and everything GitHub-specific stays in `internal/github` and `cmd/notploy-action`.

## Development

Requires Go 1.24+ and Docker for the image; every tool (golangci-lint, actionlint, the action validator) runs through `go run` / `npx` with no install.

```bash
git clone https://github.com/skygenesisenterprise/notploy-actions
cd notploy-actions
make check
```

### Root commands

| Command | Purpose |
| --- | --- |
| `make check` | What CI runs: gofmt, vet, test, lint, `action.yml` validation, actionlint |
| `make test` | `go test ./... -count=1` |
| `make lint` | `golangci-lint` v1.64.8, pinned, via `go run` |
| `make fmt` | `gofmt -w cmd internal` |
| `make build` | `bin/notploy-action` |
| `make image` | The container image the runner builds from `action.yml` |
| `make actionlint` | Lint the workflows of this repository |
| `make validate` | Validate `action.yml` with `@action-validator/cli` |

### Running the action outside a runner

Inputs map one-to-one onto environment variables (`endpoint` → `INPUT_ENDPOINT`; hyphens are accepted as-is or underscored: `INPUT_API-KEY` / `INPUT_API_KEY`):

```bash
INPUT_ENDPOINT=https://notploy.example.com \
INPUT_API-KEY=npct_xxx \
INPUT_APPLICATION-ID=my-app \
go run ./cmd/notploy-action
```

`GITHUB_OUTPUT`, `GITHUB_STEP_SUMMARY` and the `GITHUB_*` context are optional outside a runner; without them the action runs normally and skips the files. A dev container that runs `go vet` and the tests on create is provided in [`.devcontainer/`](./.devcontainer).

### Tests

The suite is deterministic: no network, no credentials. It covers the configuration validation, the GitHub-specific plumbing, the HTTP client against `httptest` servers, the deployment lifecycle with fakes, and an end-to-end test that builds the real binary and runs it against a fake Notploy instance exposing both API surfaces.

```bash
go test ./... -count=1
```

## FAQ

### Is it free?

Yes. The action is open source under the MIT License, like the Notploy platform itself. A commercial edition with additional features and premium support is maintained by [Sky Genesis Enterprise](https://skygenesisenterprise.com).

### Which Notploy API does it use?

Both. The CI/CD API (`/api/cicd/…`) is preferred because it returns the deployment id directly, supports idempotency keys and correlates back to the GitHub run. The classic endpoints (`application.deploy`, `deployment.all`) remain supported through `api-mode: legacy` and the automatic fallback. The exact contract is documented in [`docs/api-contract.md`](./docs/api-contract.md).

### Why a container action?

So the runtime is reproducible and self-contained: the Go binary is compiled when the runner builds the image from `action.yml`, and the runner needs neither Go nor Node.js. The trade-off is that self-hosted runners need Docker.

### Does it use GITHUB_TOKEN?

Only if you pass it. `GITHUB_TOKEN` is never read implicitly; the `github-token` input is used exclusively to publish the commit status, and the job needs `statuses: write` for that. Without it the job can keep `permissions: { contents: read }`.

### How do I get help or report a bug?

Join the [Notploy Discord](https://discord.gg/GUkDugVh34) for questions, open a [GitHub issue](https://github.com/skygenesisenterprise/notploy-actions/issues) for bugs and feature requests, or check [`docs/troubleshooting.md`](./docs/troubleshooting.md) for the common failure modes and their hints. Never report a security vulnerability in a public issue — see [Security](#security).

## Related resources

### Official

- Notploy platform: [github.com/skygenesisenterprise/notploy](https://github.com/skygenesisenterprise/notploy)
- Product docs: [docs.notploy.com](https://docs.notploy.com)
- Website: [notploy.com](https://notploy.com)
- Hosted platform: [app.notploy.com](https://app.notploy.com)
- Templates gallery: [templates.notploy.com](https://templates.notploy.com)

### Community

- Questions and discussions: [Notploy Discord](https://discord.gg/GUkDugVh34)
- Bugs and feature requests: [GitHub Issues](https://github.com/skygenesisenterprise/notploy-actions/issues)
- Contribution guide: [`.github/CONTRIBUTING.md`](./.github/CONTRIBUTING.md)

### Commercial

- Commercial edition and premium support: [Sky Genesis Enterprise](https://skygenesisenterprise.com)

## Contributing

Contributions and suggestions are welcome. Please read the [Code of Conduct](./CODE_OF_CONDUCT.md) before participating, then follow the [contribution guide](./.github/CONTRIBUTING.md) for setup and review expectations.

- Keep PR titles in [Conventional Commits](https://www.conventionalcommits.org) format (`fix: …`, `feat: …`, `docs: …`).
- Run `make check` before opening a pull request.
- Any change to `action.yml` must be mirrored in `internal/config`, `README.md` and the end-to-end test — the input/output contract is what consumers pin to `@v1`.

## Security

Do not report security vulnerabilities in public issues. The action masks `api-key` and `github-token` in the logs, never writes them to outputs or error messages, and never prints request headers; see [`docs/security.md`](./docs/security.md) for the full picture. Follow the private reporting instructions in [SECURITY.md](./SECURITY.md).

## License

The project is licensed under the [MIT License](./LICENSE).

## Note

Notploy Actions is an open-source project maintained by [Sky Genesis Enterprise](https://skygenesisenterprise.com). Links to external services are provided for convenience and do not imply endorsement. Check each service's own terms before relying on it in production.
