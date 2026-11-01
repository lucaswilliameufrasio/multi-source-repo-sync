# Multi-Source Repo Sync

Keeps the same repositories living in several GitHub organizations in step with each other, safely. It is a command line tool with a local web page, built for the case where no copy is the canonical one (different teams or partners each own a copy and every one of them may be ahead).

**It never merges, never force-pushes and never deletes.** Every write is a **new** branch and a pull request in the repository that receives the changes, made only after you type an exact confirmation phrase, and planned again right after you do.

## Install

Linux and macOS:

```sh
curl -fsSL https://github.com/lucaswilliameufrasio/multi-source-repo-sync/releases/latest/download/multi-source-repo-sync-installer.sh | sh
```

Windows PowerShell:

```powershell
Invoke-WebRequest https://github.com/lucaswilliameufrasio/multi-source-repo-sync/releases/latest/download/multi-source-repo-sync-installer.ps1 -OutFile multi-source-repo-sync-installer.ps1
.\multi-source-repo-sync-installer.ps1
```

The installers verify the SHA-256 checksum of the download. Archives for Linux, macOS and Windows (amd64 and arm64) are on the [releases page](https://github.com/lucaswilliameufrasio/multi-source-repo-sync/releases), each with its `.sha256`. From source: `go install github.com/lucaswilliameufrasio/multi-source-repo-sync/cmd/multi-source-repo-sync@latest`.

It needs `git` 2.38 or newer and the GitHub CLI (`gh`) logged in to the organizations you use; it never reads or stores a token.

## Quick start

```sh
mkdir -p ~/.config/multi-source-repo-sync
curl -fsSL https://raw.githubusercontent.com/lucaswilliameufrasio/multi-source-repo-sync/main/config/repos.example.json \
  -o ~/.config/multi-source-repo-sync/repos.json     # then edit it: your organizations and services

multi-source-repo-sync status      # what each copy has (read-only)
multi-source-repo-sync serve       # the same, on a local web page
```

The manifest is looked for in `--config`, then `$MULTI_SOURCE_REPO_SYNC_CONFIG`, then `config/repos.json`, then `~/.config/multi-source-repo-sync/repos.json`.

## Organizations and pairs

The manifest names any number of organizations with a short label each ([`config/repos.example.json`](config/repos.example.json), `"version": 1`). The unit of synchronization is the **pair**: by default every pair of organizations, or only the ones listed in `"pairs"`. `"organization_services"` says which services an organization has when it does not have all of them (in the example, Initech-Org has only `api-gateway` and `worker-service`). See [ADR-004](docs/adr/004-generic-repository-groups.md).

- `status` and `plan` report every service in every pair; `--pair left/right` limits them to one.
- `serve` opens the web interface (below).
- `sync` works on one service, one branch and one pair; `--pair` is required when the service is in several. The typed confirmation names the pair: `sync <service> <branch> <left/right>`.
- `resolve prepare` takes `--pair` and `--target <label>` (the organization that receives the PR); the PR is opened by `publish` after the phrase `publish <service> <branch> <source>-to-<target>`.
- Candidate branches are named `sync/multi-source-repo-sync/<source>-to-<target>/<branch>/<shas>`.


## Current scope

- `status` inventories branches and open pull requests on both sides; it is read-only.
- `plan` also checks commit ancestry and merge-tree conflicts; it is read-only.
- `sync` defaults to a dry run for one service and branch. `sync --apply` can create new integration branches and pull requests after an exact typed confirmation.
- `resolve` prepares a persistent, local three-way merge for conflicts, records an explicitly approved local test command, and publishes the reviewed resolution as a PR only with confirmation.
- `main` and `develop` are the initial sync candidates. Other branches are inventoried but not synchronized.
- `dev-test` is preserve-only: it may be reported, but is never a sync candidate and is never deleted.
- No command merges a PR, force-pushes, updates an existing candidate branch, or deletes a branch. `sync --apply` only creates a new candidate branch and PR; opening a PR may trigger GitHub checks.

The repositories it may touch are the ones listed in the manifest (`services`), and only the branches in `sync_branches`.

## Putting a branch into the other copies

```sh
multi-source-repo-sync propagate --service api-gateway --branch feat/my-work --from initech            # dry run
multi-source-repo-sync propagate --service api-gateway --branch feat/my-work --from initech --apply   # asks for the phrase
```

For work done in one organization that the others should receive: it creates the **same branch** in every other organization that has the service (`--to a,b` limits them) and opens a pull request from it into the base branch (`--base`, default the first sync branch, `main`). It is the safe version of pushing one branch to several remotes by hand.

- It only creates. A branch of that name that already exists in a target with **other commits** is left alone and reported; one with the **same commit** only gets its pull request. Nothing is force-pushed, merged or deleted.
- The branch must share history with the target's base branch and have commits the base lacks, or there is nothing to propose.
- Only an ordinary branch can be propagated: not a sync branch (`main`, `develop`: use `sync`), not `dev-test`, not the candidates this tool makes.
- One confirmation covers all the targets and names them (`propagate <service> <branch> from <source> to <a,b>`); the plan is made again after it, and the source must still be at the commit you reviewed. A target that cannot take it (other history, no base branch, merge commits off) is skipped and the others go ahead.
- The same screen is on the web interface.

## Local web interface

```sh
multi-source-repo-sync serve            # http://127.0.0.1:7878/?token=...
multi-source-repo-sync serve --addr 127.0.0.1:0   # any free port
```

The same status, plan and sync as the commands, on one page: every service and pair at a glance, each branch a chip you can open (heads, open pull requests, what a sync would create), and a button that only unlocks after you type the exact confirmation phrase. The page is served from the binary (no build step, no network access to anything but GitHub through `gh`).

- It listens **only on a loopback address** (`serve` refuses anything else) and asks for the random token printed when it starts; the first visit turns the token into a `HttpOnly`, `SameSite=Strict` cookie. A request whose `Host` is not loopback (DNS rebinding) or whose `Origin` is another site is refused.
- It writes to GitHub only through the same workflow as `sync --apply` (`internal/workflow`): the exact phrase, a new plan made after the confirmation, a check that the heads are the ones you reviewed, the merge-commit check, and never a merge, force-push or deletion. One sync runs at a time.
- Conflicts are not resolved in the browser: the page shows the `resolve prepare` command for a local copy.

## Requirements

- Go 1.23 or newer (developed with Go 1.27.1).
- Git 2.38 or newer (`git merge-tree --write-tree` is used by `plan`).
- GitHub CLI (`gh`) authenticated for both organizations.
- Git authentication configured for HTTPS fetches to both organizations.
- Git `user.name` and `user.email` configured if a divergent history needs a two-parent merge candidate.

The tool does not call `gh auth setup-git`, store credentials, or read token values. It relies on the existing `gh` and Git authentication setup.

## Commands

### Guided use (recommended)

With the installed command, start from any directory:

```sh
multi-source-repo-sync
```

The numbered menu offers status, analysis, synchronization and conflict resolution.
It lists services from the manifest and queries which allowed branches actually
exist and differ on both sides. It explains the PR direction rather than asking
you to guess `--target`. Choose `0`, Enter or EOF to cancel; nothing is approved by
default. `multi-source-repo-sync resolve` opens the resolution menu directly; plain
`sync` and `resolve prepare` also guide missing selections when no parameters are
supplied. `--help` shows the command interface without starting a workflow.

Resume lists valid local resolutions under `~/git-worktrees/<service>/`; select by
number instead of copying paths. Custom workspaces can be supplied explicitly via
the menu or `guide --workspace /path/to/workspace`. The selected workspace stays
active for conflict status, diff review, individual reviewed-file staging, check
execution, PR preview and publication. You still edit semantic conflicts in your
editor; the menu does not silently select ours/theirs.

Check menus offer **fixed suggestions** for Go (`go.mod`) and Rust (`Cargo.toml`),
or let you enter an executable and literal arguments. No command/script is read
from repository contents or saved state and executed automatically. Select a
timeout and explicitly type `executar` after the local-execution warning. These
suggestions do not claim to be the repository's full quality gate. Publication
still requires review approval and the existing exact typed confirmation.

### Explicit commands (automation)

Run from the repository root:

```sh
go run ./cmd/multi-source-repo-sync status
go run ./cmd/multi-source-repo-sync plan
go run ./cmd/multi-source-repo-sync plan --format json
go run ./cmd/multi-source-repo-sync plan --verbose
go run ./cmd/multi-source-repo-sync sync --service api-gateway --branch develop
```

Use `--config path/to/repos.json` to select another validated manifest. `plan` fetches the configured branch histories into an ephemeral local bare repository, verifies that fetched heads still match the GitHub API snapshot, checks ancestry and merge-tree conflicts, then removes its temporary data.

`sync` requires `--service` and `--branch` and is a dry run by default. To apply, add `--apply` and type the exact phrase shown after reviewing the proposed targets. Before writing, the tool re-reads both repositories and rechecks the plan. It verifies that each target repository allows merge commits before pushing anything there, then pushes only newly named candidate branches (without a force option) and opens or reuses PRs within each target repository. Divergent clean histories get a two-parent merge commit; the PR asks reviewers to use GitHub's merge-commit method, not squash or rebase. The tool never merges PRs.

Unrelated history, conflicts, stale branch snapshots, missing repositories/branches, or unavailable authentication stop the flow. A one-sided branch is inventoried and preserved; the tool does not create its counterpart automatically. If a push succeeds but PR creation fails, the candidate branch is deliberately left in place, reported, and safe to retry; the tool never rolls back by deleting it.

## Assisted conflict resolution (Linux)

`sync` still stops on conflicts. Use `resolve` to prepare a **separate local copy**
without changing either base branch or opening a PR:

```sh
multi-source-repo-sync resolve prepare --service worker-service --branch main --target globex
```

Choose `--target globex` or `--target acme` explicitly. The CLI prints a new path
under `~/git-worktrees/<service>/resolve-...`; it never overwrites an existing
directory. `--workspace /persistent/new/path` can override the location. A failed
preparation leaves partial local files for inspection; no automatic cleanup or push.
Global hooks, custom/union merge drivers and checkout filters are not accepted as
automatic conflict-resolution strategies. Ordinary Git three-way merging combines
non-conflicting edits. Textual success does **not** prove semantic correctness.

Replace `<workspace>` below with the printed path (quotes are recommended):

```sh
multi-source-repo-sync resolve status --workspace "<workspace>"
git -C "<workspace>" status
git -C "<workspace>" diff
git -C "<workspace>" diff --cached
```

Review **all** changes, not just conflict markers, including code specific to an
organization. Edit unresolved files, then stage only reviewed paths with
`git -C "<workspace>" add -- path/to/file`. For intentional deletions, use `git rm`.
Binary and modify/delete conflicts require an explicit decision too. Do not commit,
rebase, or reset this workspace: publishing creates the preserving merge commit.
The base, target and source versions remain available in the Git objects/index.

Run an explicit check **after** trusting and reviewing the combined code:

```sh
multi-source-repo-sync resolve check --workspace "<workspace>" --trusted-local -- go test ./...
```

This is **not sandboxed**: the command has your local filesystem/network permissions
and inherited developer environment. Never opt in for untrusted code. No command
is selected from repository files or saved state, and no shell interpolation is
performed. Linux process-group cancellation handles ordinary child processes;
deliberately detached processes are not contained. The timeout defaults to 5 minutes
and can be set before `--`, for example `--timeout 20m`.

The CLI checks unresolved files, markers, unstaged and untracked work, records only
the executable name/argument digest and tested tree, and invalidates success after
a failed retry or changed tree. Ignored build artifacts are not included in the
candidate. A successful command is only evidence for that check, **not proof that
the complete project quality gate or every behavior passed**. Supply the project's
full gate as one explicit executable/script if multiple steps are required.

Preview, then publish only when satisfied with the diff and check coverage:

```sh
multi-source-repo-sync resolve publish --workspace "<workspace>"
multi-source-repo-sync resolve publish --workspace "<workspace>" --apply --reviewed
```

Publication requires typing `publish <service> <branch> <target>` exactly. It
rechecks both remote heads and merge-commit support before pushing. If either head
moved, keep the old workspace for comparison and prepare a fresh one; the CLI never
rebases or overwrites your work. A new commit has **both original heads as parents**.
Candidate branches are never updated, force-pushed or deleted. Failed PR creation
leaves the candidate intact; repeat `publish` to reuse it. The `.git` recovery file
is local task data, not an authorization or tamper-proof security boundary. Concurrent
check/publish calls are refused; inspect any stale `.git/multi-source-resolution.lock`
after a crash before manually removing it.

Merge the reviewed PR using **merge commit**, not squash/rebase. No automatic merge
occurs. Then run `plan` again before synchronizing in the other direction. `main`
and `develop` are separate resolutions; `dev-test` stays preserve-only.

## Output and checks

The default text output summarizes only synchronization candidates, with short
human-readable statuses and PR counts. `--verbose` retains the full branch
inventory, hashes, and PR details. `--format json` keeps the structured report
unchanged. Neither display mode changes synchronization or preservation rules.

```sh
gofmt -l .
golangci-lint run ./...
go vet ./...
go test -count=1 ./...
go build ./...
```

Tests use temporary local Git repositories; they do not contact GitHub or a development environment. See [`TESTING_STANDARD.md`](TESTING_STANDARD.md) and [ADR-001](docs/adr/001-safe-bidirectional-sync.md) for the verification and synchronization rules.
