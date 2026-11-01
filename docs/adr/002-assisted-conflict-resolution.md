# ADR-002: Assisted conflict resolution in a persistent workspace

**Date:** 2026-10-25
**Status:** Accepted
**Evidence:** User requested the assisted resolution flow described in this conversation.

## Decision

Extend, rather than bypass, ADR-001. `resolve prepare` creates a new persistent
checkout for one service, branch and explicit target side. Git's normal three-way
merge combines non-conflicting edits; unmerged files remain explicit human
decisions, including binary and modify/delete conflicts. Never select ours/theirs,
use a union merge, rebase, or claim semantic equivalence from textual success.

`resolve status` resumes the workspace and lists conflicts. The operator reviews
the merge, edits remaining conflicts and stages the reviewed result. `resolve check`
executes only an explicitly supplied command with an explicit trusted-local
execution opt-in. No commands from repository metadata are auto-discovered or run.
This is ordinary local developer execution, **not a sandbox**; it has the user's
filesystem/network authority. Untrusted repositories must be tested separately in
an appropriate sandbox, not opted into this mode. Record only the executable name,
argument digest, success and the exact Git tree, not raw arguments, output or
environment. Tests modifying tracked files invalidate their result. Recheck conflict markers, unmerged entries,
unstaged/untracked changes and the recorded tree before publishing.

`resolve publish` previews by default. Remote writes require `--apply`, explicit
review confirmation, and a typed phrase scoped to service/branch/target. Recheck
both remote heads and merge-commit support, then publish a new two-parent commit
and open/reuse a PR. Neither base branch is updated. Existing candidate branches
are never rewritten, even with a non-force fast-forward. Failed PR creation leaves
the candidate intact and permits an identical retry. The original histories remain
reachable; preservation of behavior still requires human review and suitable tests.

## Alternatives and consequences

Automatic ours/theirs or union resolution was rejected: it preserves neither
intent nor necessarily valid code. Running inferred test scripts was rejected:
repository content is not execution approval. Workspaces and partial progress
remain on disk, with no automatic cleanup or recovery that runs tests/pushes.
One target is published at a time; reverse synchronization is replanned after merge.
Revisit if introducing an AI resolver, sandbox, automatic test selection or automatic merge.
