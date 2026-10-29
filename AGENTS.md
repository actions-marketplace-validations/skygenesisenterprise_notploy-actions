# Agent Guidelines

Repository: **notploy-actions** — the official GitHub Action for
[Notploy](https://github.com/skygenesisenterprise/notploy), written in Go and
shipped as a container action.

## Commands

```bash
make check    # what CI runs: fmt-check, vet, test, lint, action.yml validation, actionlint
make fmt      # gofmt -w cmd internal
make test     # go test ./... -count=1
make lint     # golangci-lint v1.64.8 (via go run, pinned)
make build    # bin/notploy-action
make image    # docker build of the action image
```

Individual tools (no install needed, everything runs through `go run`/`npx`):

- `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7` — lint workflows.
- `npx -y @action-validator/cli@0.6.0 action.yml` — validate `action.yml`.
  The unrelated npm package named `action-validator` is **not** the right tool.

## Code style

- Go with the standard library only: the module has **zero third-party
  dependencies** (no `go.sum`). Never add a dependency without discussion.
- Keep `internal/notploy` and `internal/deployment` free of GitHub-specific
  code and environment reads; they take parameters and a logger. Everything
  touching `GITHUB_*`/`INPUT_*` belongs in `internal/github` or
  `cmd/notploy-action`.
- Module path: `github.com/skygenesisenterprise/notploy-actions`; internal
  imports use the `internal/...` prefix.
- Conventions: `gofmt` is law, no `init()`, explicit error handling, table-driven
  tests, comments explain *why* and document exported API only.
- Prefer interfaces over concrete types at package boundaries when it makes a
  test simpler; do not introduce interfaces speculatively.
- Errors that reach the user must be actionable: use the `Hint()` branches in
  `internal/notploy/errors.go` rather than ad-hoc messages, and never leak
  credentials or raw request headers.

## Input/output contract

- `action.yml` is the single source of truth for inputs and outputs. Any change
  there must be mirrored in `internal/config` (or `cmd/notploy-action` for
  outputs), in `README.md`, and in the end-to-end test
  `cmd/notploy-action/main_test.go`.
- Never rename or repurpose an existing input/output without a major-version
  consideration: consumers pin `@v1`.
- Do not log values of `api-key` or `github-token`; masking happens first in
  `main.go`, and tests assert redaction.

## API reference

- Before touching client code, read [`docs/api-contract.md`](./docs/api-contract.md):
  it documents the exact endpoints, payloads and status semantics the action
  relies on (CI/CD surface *and* legacy surface).
- The action supports two API surfaces (`internal/notploy/cicd.go`,
  `internal/notploy/client.go`) with a strict fallback rule in
  `internal/deployment` — see `docs/api-contract.md#api-modes`.

## Testing

```bash
go test ./... -count=1        # fast, deterministic, no network
go test ./cmd/... -count=1    # includes the end-to-end binary test (slower)
```

- The end-to-end test builds the real binary and runs it against a fake
  Notploy server; extend that server rather than adding one-off mocks.
- New behaviour needs a test: config validation, fallback rules, correlation,
  settling, outputs, summary lines.
- Workflows must pass `actionlint`; `action.yml` must pass the action
  validator.

## Repository hygiene

- Documentation lives in `README.md`, `docs/` and `.github/`; keep them in sync
  with behaviour changes.
- Ready-to-copy workflows live in `examples/` and are linted by `actionlint`.
- No Node.js tooling: there is no `package.json`; do not add pnpm/npm files,
  husky hooks, changesets or Storybook — leftovers from other repositories.
