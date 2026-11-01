# Testing Standard

## Verified local contract

- Go toolchain available during initial setup: Go 1.27.1; module compatibility target: Go 1.23.
- Git is required by history-inspection tests and by the `plan` command.
- `gh` is needed for live CLI use, but tests do not call it or GitHub.
- No Docker, database, or other local service is required.
- Git integration tests create disposable working and bare repositories under the operating system temporary directory, verify local candidate-branch pushes, and remove them after each test. They do not use GitHub credentials or contact the network.
- Resolution tests exercise real three-way conflicts and local bare remotes,
  preserving both parents, stale heads, marker/dirty-tree rejection, retries and
  candidate collisions. CLI checks use explicit synthetic child commands; they do
  not run service test suites or auto-discover scripts. Trusted-local check
  execution is Linux-only and is not a security sandbox.
- Guided CLI tests cover invalid/EOF/blank choices, available-branch filtering,
  explicit target selection, workspace discovery, literal command arguments and
  shared-buffer confirmation. A full local journey resumes a real Git workspace,
  reviews/stages a file, runs a synthetic test child and previews a PR without
  accessing GitHub or publishing. Fixed Go/Rust suggestions require user approval;
  tests never execute scripts discovered in service repositories.

## Quality checks

Run from the repository root:

```sh
gofmt -l .
golangci-lint run ./...
go vet ./...
go test -count=1 ./...
go build ./...
```

There is no integration build tag: tests that exercise Git use the real local Git executable as part of the normal test suite. The `sync --apply` operation is not executed against the real service remotes during tests. Do not run tests against Acme or Globex GitHub repositories; live API/fetch/push/PR operations belong only to explicit CLI use after confirmation.
