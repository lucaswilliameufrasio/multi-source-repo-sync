# ADR-004: Synchronize any number of organizations, one pair at a time

**Date:** 2026-11-01
**Status:** Accepted
**Evidence:** User request in the project conversation: make the tool generic, so that improvements made in the personal organization (Initech-Org) reach Acme-Org and Globex-Org the same way.

## Context

ADR-001 fixed two sides, Acme and Globex, in the manifest, the report, the Git inspection, the pull-request executor and the guided flows. The same services now live in a third organization, and the work done there has to flow to the other two. The three copies must stay separate (different partners): there is no single canonical repository.

## Decision

- The manifest names **any number of organizations** (at least two), each with a short label and a GitHub owner: `"organizations": {"initech": "Initech-Org", "acme": "Acme-Org", "globex": "Globex-Org"}`. Version 1 manifests (exactly `acme` and `globex`) keep working.
- The unit of synchronization stays the **pair**, exactly as in ADR-001. By default every pair of organizations is a pair; an optional `"pairs"` list restricts them. A pair has a left and a right side (the order of the manifest); every rule of ADR-001 holds per pair.
- Reports are produced per service and per pair. `sync` works on one service, one branch and **one pair** (`--pair left/right`, required when there is more than one pair), so a typed confirmation still covers exactly one set of remote writes.
- Candidate branches name both labels (`sync/multi-source-repo-sync/<source>-to-<target>/<branch>/<shas>`), so pairs never collide.
- Nothing about safety changes: no force-push, no deletion, no automatic merge, `dev-test` preserve-only, SHA recheck before pushing.

## Alternatives considered

- **A star around one canonical organization:** rejected, because no organization is canonical; each partner group must be able to receive and give changes.
- **N-way merge in one operation:** rejected. It would put several repositories' writes behind one confirmation and make a conflict in one pair block the others. Pairs fail independently and are reviewed independently.
- **Keep two sides and run the tool three times with three manifests:** rejected; the user would maintain three copies of the same list and nothing would show the whole picture.

## Consequences

- JSON output changes shape (`left`/`right` sides with a `label`, a `pair` per entry); there are no known consumers other than the user's own scripts.
- The number of pairs grows quadratically with the organizations (three organizations: three pairs). That is acceptable for the expected size.
- A later local web interface (`serve`) reads the same report; it does not change these rules.

## Revisit when

A synchronization needs to change more than two repositories atomically, or the number of organizations makes the per-pair view unwieldy.

## Rename

The tool was called `acme-globex-sync` while it knew two organizations. With any number of them the name was misleading, so it is now **multi-source-repo-sync** (module path, binary, candidate-branch prefix, resolution files). ADR-001 keeps the old name because it records what was decided then. Resolution workspaces prepared before the rename (`acme-globex-resolution.*`) are not picked up; finish or discard them first.

## Local web interface

The command line has `serve`, a web page on a loopback address. It was chosen over a desktop application (packaging and updates for one user) and over a hosted web application (it needs the user's `gh` and Git credentials, which must not leave the machine). The safety of `sync` moved into `internal/workflow` so that the command line and the page share one implementation of the rules: exact confirmation phrase (service, branch and pair), a new plan after the confirmation, the reviewed heads compared with the fresh ones, the merge-commit check, one write at a time. The page adds its own protections: loopback only, a random token turned into a `SameSite=Strict` cookie, `Host` and `Origin` checks, JSON-only posts, a strict Content-Security-Policy, and data rendered as text.

## Propagating a branch to the other copies

`propagate` puts one ordinary branch of one organization into every other copy of the service and opens a pull request into each base branch. It replaces pushing one branch to several remotes by hand, which is how the same improvement reached the three copies of the api-gateway. Unlike `sync`, which compares two heads of the same branch and merges their histories in a candidate branch, it puts the source's commit under the same name in the targets, so the pull request is the source's own work.

The confirmation rule of ADR-001 says one typed confirmation covers exactly one set of remote writes. A propagation is one set: the phrase names the service, the branch, the source and **every target** (`propagate api-gateway feat/x from initech to acme,globex`), so a phrase for fewer targets does not authorize more. Targets fail independently: one that cannot take the branch is skipped or reported as failed and the others still go ahead, because they are separate repositories and a refusal in one says nothing about the other. Nothing is rolled back, as in `sync`.
