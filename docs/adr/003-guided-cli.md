# ADR-003: Guided CLI without implicit approval

**Date:** 2026-10-27
**Status:** Accepted
**Evidence:** User requested that the CLI guide usage and expose valid choices.

## Decision

Running without arguments opens a numbered guide, also available as `guide`.
Keep explicit flags and JSON interfaces for automation. The guide lists services
from the validated manifest, queries branch availability on both sides after
service selection, and offers only allowed, present, differing branches. Explain
the PR target by organization and direction; it never selects a winner.

Discover resumable resolution workspaces under the configured user's
`~/git-worktrees/<service>/` without recursively searching the whole filesystem.
Validate recovery identity against the manifest, ignore symlink directories, and
keep an explicit-path escape hatch for custom workspaces. The selected path stays
in the session for review, staging, checks, preview and publication.

The guide delegates to existing operations rather than bypassing their checks.
Review diffs without external diff/textconv helpers, stage only one explicitly
selected reviewed path, and propose fixed test commands based on conventional
manifest filenames. These are suggestions, not repository-provided executable
instructions or a claim about the complete quality gate. Execution requires a
separate informed approval. Custom commands are entered as executable and literal
arguments, not interpolated shell text. Never rerun commands from saved metadata.

Menu selection is not publication approval. Preserve explicit review approval,
dry-run preview and the existing exact typed publication phrase. Blank input,
zero and EOF never authorize a write or test. Use one shared input buffer across
menus and confirmation prompts, so piped input cannot be consumed or lost by
nested buffered readers. No new dependencies or terminal-control sequences.

## Consequences

Users do not need to memorize flags, valid names, target direction or workspace
paths. Editing conflicting code still requires a real editor/review; the guide
does not invent a semantic solution. GitHub reads require authentication; failure
stops selection rather than guessing branches. The guide works with numbered
lines in an ordinary terminal and exits safely without input.
