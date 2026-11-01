# Contributing to multi-source-repo-sync

Thanks for considering a contribution. This explains how to set the project up and what a pull request needs.

## Getting Started

Prerequisites: [mise](https://mise.jdx.dev) (recommended) or Go, and `git` 2.38 or newer. Tests use the real `git` on disposable local repositories; they never call GitHub or need a token.

```bash
make help     # list every target
```

## Development Loop

```bash
make fmt       # format
make vet       # go vet
make lint      # golangci-lint
make test      # the whole test suite
make quality   # the gate before a pull request: fmt-check, vet, lint, test
make snapshot  # build every release archive locally, without publishing
```

`make quality` must pass before you open or update a pull request. CI runs the same checks.

## Pull Requests

- Keep PRs small and focused; one logical change per PR.
- Add or update tests for any behavior change. Anything that writes to a repository needs a test of what it refuses to write, not only of what it writes.
- Update `README.md` or `docs/` when behavior or the manifest changes. A decision that is costly to undo gets an ADR in `docs/adr/`.
- Commits follow [Conventional Commits](https://www.conventionalcommits.org) (`feat:`, `fix:`, `docs:`, `test:`, `chore:`, `refactor:`, `ci:`): the changelog is generated from them.

## The rules the tool keeps

A change must not break these, and a change that relaxes one needs an ADR first:

- never merge, force-push, delete a branch, or update an existing branch;
- every write is preceded by an exact typed confirmation and a new plan;
- `dev-test` is never synchronized or deleted;
- `serve` listens on loopback only.

## Releasing

Maintainers: run the **Prepare Release** workflow with the new version; it opens a pull request that updates `CHANGELOG.md`. After it is merged, push the tag `vX.Y.Z`: the **Release** workflow tests, builds the archives for Linux, macOS and Windows, and publishes the release with the installers and checksums.

## Security

Never commit or paste credentials. If you find a vulnerability, follow [`SECURITY.md`](SECURITY.md) instead of opening an issue.
