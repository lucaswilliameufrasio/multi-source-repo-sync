# ADR-001: Safe bidirectional synchronization between service repositories

**Date:** 2026-10-21
**Status:** Accepted
**Evidence:** User-confirmed requirements in the project conversation.

## Context

The same generic services are maintained in separate private repositories under Acme-Org and Globex-Org. Their histories may contain commits exclusive to either side. The repositories are not configured as forks, and adding cross-organization remotes to every service clone is undesirable. In particular, losing Globex-only commits is unacceptable.

## Decision

- Keep a standalone Go CLI in its own repository, `multi-source-repo-sync`.
- Load paired repository names and the allowlist from a versioned manifest. Use the existing `gh` and Git authentication; do not store tokens in source or configuration.
- Treat synchronization as bidirectional. Preserve both histories and, when synchronization is implemented, propose pull requests within each affected repository rather than overwriting one side.
- Never force-push or delete branches. Stop when histories have no common ancestor, merge conflicts occur, or fetched heads do not match the inventory snapshot.
- Inventory `dev-test`, but make it preserve-only: never synchronize or delete it.
- Initially plan `main` and `develop`; inventory all other branch names and open PRs without automatically synchronizing them.
- Keep `status` and `plan` read-only. `sync` is a dry run by default and operates on one allowlisted service/branch at a time.
- Require `sync --apply` plus the exact typed confirmation phrase before remote writes. Re-read the API snapshot after confirmation and verify the branch SHAs again immediately before pushing.
- For each affected target repository, create a deterministic, new candidate branch containing the target history and incoming source history; never update an existing branch. Push without force and open or reuse a PR in that same repository. Never auto-merge or delete.
- For divergent but mergeable histories, create a two-parent merge commit in an ephemeral local bare repository. Require the configured Git identity and verify the target repository allows merge commits before pushing; instruct PR reviewers to use a merge commit rather than squash/rebase so original commits remain reachable.
- If a push succeeds but a later PR step fails, report the partial result and leave the new branch in place; never attempt destructive rollback. A retry reuses an identical candidate branch and open PR.

## Alternatives considered

- Add a second remote to every service clone: rejected because it spreads configuration across clones and couples the workflow to each checkout.
- Mirror one repository over the other: rejected because it can erase commits and branches that exist on only one side.
- Select a single canonical organization immediately: rejected because the user wants both sides editable and synchronized bidirectionally.
- Automatically merge PRs or push directly to `main`/`develop`: rejected; a reviewed PR is the integration boundary.

## Consequences

- The manifest lists the services the tool may touch.
- `plan` needs read access to both GitHub repositories and Git fetch authentication. It uses an ephemeral local bare repository so service checkouts are untouched.
- A clean history comparison is only a proposal. Remote writes require explicit, per-branch confirmation and may trigger CI checks.
- Partial failures may leave a candidate branch without a PR; this is safer than deleting or force-updating remote state and is recoverable by retry or manual review.
- Branches present on only one side are preserved and reported; no counterpart is created automatically.

## Revisit when

The user changes the sync branch set, allowlist, PR workflow, or preservation rules; or when a safe PR candidate cannot be represented as a branch within the target repository.
