package gitinspect

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ResolutionState is recovery data, never authority to run commands or select remotes.
type ResolutionState struct {
	Version int    `json:"version"`
	Service string `json:"service"`
	Branch  string `json:"branch"`
	Target  string `json:"target"`
	// Source is the label of the organization the changes come from.
	Source    string   `json:"source"`
	TargetSHA string   `json:"target_sha"`
	SourceSHA string   `json:"source_sha"`
	Tree      string   `json:"checked_tree,omitempty"`
	Check     []string `json:"check_command,omitempty"`
	Candidate string   `json:"candidate_commit,omitempty"`
}

type ResolutionStatus struct {
	State     ResolutionState
	Conflicts []string
}

var organizationLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func resolutionFile(directory string) string {
	return filepath.Join(directory, ".git", "multi-source-resolution.json")
}

func LoadResolution(directory string) (ResolutionState, error) {
	var state ResolutionState
	info, err := os.Lstat(filepath.Join(directory, ".git"))
	if err != nil || !info.IsDir() {
		return state, fmt.Errorf("expected a dedicated resolution checkout with a .git directory")
	}
	data, err := os.ReadFile(resolutionFile(directory))
	if err != nil {
		return state, fmt.Errorf("read resolution state: %w", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("decode resolution state: %w", err)
	}
	if state.Version != 1 || state.Service == "" || state.Branch == "" || state.Branch == "dev-test" ||
		!organizationLabel.MatchString(state.Target) ||
		!organizationLabel.MatchString(state.Source) || state.Source == state.Target ||
		!validCommitSHA(state.TargetSHA) || !validCommitSHA(state.SourceSHA) ||
		(state.Tree != "" && !validCommitSHA(state.Tree)) || (state.Candidate != "" && !validCommitSHA(state.Candidate)) {
		return state, fmt.Errorf("invalid resolution identity")
	}
	return state, nil
}

func SaveResolution(directory string, state ResolutionState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Join(directory, ".git"), "resolution-state-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() {
		_ = os.Remove(name)
	}()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(name, resolutionFile(directory))
}

// PrepareResolution never writes to a remote or reuses an existing directory.
func (i GitInspector) PrepareResolution(ctx context.Context, directory string, state ResolutionState, targetURL string, sourceURL string) (ResolutionStatus, error) {
	if state.Branch == "dev-test" || !validCommitSHA(state.TargetSHA) || !validCommitSHA(state.SourceSHA) {
		return ResolutionStatus{}, fmt.Errorf("invalid resolution branch or heads")
	}
	if _, err := i.run(ctx, "check-ref-format", "refs/heads/"+state.Branch); err != nil {
		return ResolutionStatus{}, fmt.Errorf("invalid resolution branch")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return ResolutionStatus{}, err
	}
	directory = absolute
	if err := os.Mkdir(directory, 0o700); err != nil {
		return ResolutionStatus{}, fmt.Errorf("create new workspace (existing directories are never reused): %w", err)
	}
	// Empty template avoids executing user hooks copied by git init.
	if _, err := i.run(ctx, "init", "--quiet", "--template=", directory); err != nil {
		return ResolutionStatus{}, err
	}
	for _, setting := range [][]string{{"core.hooksPath", "/dev/null"}, {"core.fsmonitor", "false"}, {"core.attributesFile", "/dev/null"}, {"merge.conflictStyle", "diff3"}, {"merge.default", "text"}, {"merge.renormalize", "false"}, {"rerere.enabled", "false"}, {"rerere.autoupdate", "false"}} {
		if _, err := i.run(ctx, "-C", directory, "config", "--local", setting[0], setting[1]); err != nil {
			return ResolutionStatus{}, err
		}
	}
	heads := []BranchHeads{{Name: state.Branch}}
	if err := i.fetch(ctx, directory, "target", targetURL, heads); err != nil {
		return ResolutionStatus{}, err
	}
	if err := i.fetch(ctx, directory, "source", sourceURL, heads); err != nil {
		return ResolutionStatus{}, err
	}
	for side, expected := range map[string]string{"target": state.TargetSHA, "source": state.SourceSHA} {
		actual, err := i.resolve(ctx, directory, "refs/remotes/"+side+"/"+state.Branch)
		if err != nil || actual != expected {
			return ResolutionStatus{}, fmt.Errorf("%s branch changed while preparing; workspace preserved at %s", side, directory)
		}
	}
	if _, err := i.run(ctx, "-C", directory, "merge-base", state.TargetSHA, state.SourceSHA); err != nil {
		return ResolutionStatus{}, fmt.Errorf("cannot resolve unrelated histories")
	}
	alreadyIntegrated, err := i.isAncestor(ctx, directory, state.SourceSHA, state.TargetSHA)
	if err != nil {
		return ResolutionStatus{}, err
	}
	if alreadyIntegrated {
		return ResolutionStatus{}, fmt.Errorf("target already contains source history; run plan instead")
	}
	for _, sha := range []string{state.TargetSHA, state.SourceSHA} {
		if err := i.checkResolutionAttributes(ctx, directory, sha); err != nil {
			return ResolutionStatus{}, err
		}
	}
	if _, err := i.run(ctx, "-C", directory, "checkout", "--quiet", "--detach", state.TargetSHA); err != nil {
		return ResolutionStatus{}, err
	}
	state.Version = 1
	state.Tree, state.Check, state.Candidate = "", nil, ""
	if err := SaveResolution(directory, state); err != nil {
		return ResolutionStatus{}, err
	}
	_, mergeErr := i.run(ctx, "-C", directory, "merge", "--no-commit", "--no-ff", "--no-edit", state.SourceSHA)
	status, err := i.ResolutionStatus(ctx, directory)
	if err != nil {
		return status, err
	}
	if mergeErr != nil && (exitCode(mergeErr) != 1 || len(status.Conflicts) == 0) {
		return status, fmt.Errorf("merge preparation failed; workspace preserved: %w", mergeErr)
	}
	return status, nil
}

// Custom/union drivers can silently discard or invent changes; filters may execute
// repository-selected code at checkout. Neither is an approved resolution strategy.
func (i GitInspector) checkResolutionAttributes(ctx context.Context, directory string, sha string) error {
	index := filepath.Join(directory, ".git", "resolution-attributes-index")
	defer func() {
		_ = os.Remove(index)
	}()
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := i.runWithEnv(ctx, env, "-C", directory, "read-tree", sha); err != nil {
		return err
	}
	files, err := i.runWithEnv(ctx, env, "-C", directory, "ls-files", "-z")
	if err != nil {
		return err
	}
	paths := strings.Split(strings.TrimSuffix(string(files), "\x00"), "\x00")
	for start := 0; start < len(paths); start += 100 {
		end := min(start+100, len(paths))
		args := []string{"-C", directory, "check-attr", "--cached", "-z", "merge", "filter", "--"}
		args = append(args, paths[start:end]...)
		output, err := i.runWithEnv(ctx, env, args...)
		if err != nil {
			return err
		}
		fields := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
		for index := 0; index+2 < len(fields); index += 3 {
			attribute, value := fields[index+1], fields[index+2]
			allowed := value == "unspecified" || value == "unset" || (attribute == "merge" && (value == "set" || value == "text" || value == "binary"))
			if !allowed {
				return fmt.Errorf("repository uses a custom/union merge driver or checkout filter; automatic preparation is blocked, review .gitattributes first")
			}
		}
	}
	return nil
}

func (i GitInspector) ResolutionStatus(ctx context.Context, directory string) (ResolutionStatus, error) {
	state, err := LoadResolution(directory)
	if err != nil {
		return ResolutionStatus{}, err
	}
	output, err := i.run(ctx, "-C", directory, "diff", "--no-ext-diff", "--no-textconv", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return ResolutionStatus{}, err
	}
	var conflicts []string
	for _, name := range strings.Split(string(output), "\x00") {
		if name != "" {
			conflicts = append(conflicts, name)
		}
	}
	return ResolutionStatus{State: state, Conflicts: conflicts}, nil
}

// ResolutionTree requires the working copy to match the reviewed index exactly.
func (i GitInspector) ResolutionTree(ctx context.Context, directory string) (string, error) {
	status, err := i.ResolutionStatus(ctx, directory)
	if err != nil {
		return "", err
	}
	if len(status.Conflicts) > 0 {
		return "", fmt.Errorf("%d unresolved files; edit them and stage only the reviewed resolutions", len(status.Conflicts))
	}
	head, err := i.resolve(ctx, directory, "HEAD")
	if err != nil || head != status.State.TargetSHA {
		return "", fmt.Errorf("workspace HEAD changed; expected the original target, not a manual commit/rebase")
	}
	mergeHead, err := i.resolve(ctx, directory, "MERGE_HEAD")
	if err != nil || mergeHead != status.State.SourceSHA {
		return "", fmt.Errorf("merge state was aborted or changed; prepare a fresh workspace instead of discarding source changes")
	}
	if _, err := i.run(ctx, "-C", directory, "diff", "--no-ext-diff", "--no-textconv", "--exit-code", "--quiet"); err != nil {
		return "", fmt.Errorf("unstaged changes: stage the reviewed files before checking or publishing")
	}
	untracked, err := i.run(ctx, "-C", directory, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	if len(untracked) > 0 {
		return "", fmt.Errorf("untracked files: review/stage them or explicitly ignore generated artifacts")
	}
	if _, err := i.run(ctx, "-C", directory, "diff", "--no-ext-diff", "--no-textconv", "--cached", "--check"); err != nil {
		return "", fmt.Errorf("staged conflict markers or whitespace errors remain; run git diff --cached --check")
	}
	// Also detect staged markers even when the operator manually staged an unmerged file.
	_, err = i.run(ctx, "-C", directory, "grep", "--cached", "-I", "-l", "-E", "^(<<<<<<< |=======|>>>>>>> |\\|\\|\\|\\|\\|\\|\\| )", "--", ".")
	if err == nil {
		return "", fmt.Errorf("possible conflict markers in staged files; review before publishing")
	}
	if exitCode(err) != 1 {
		return "", fmt.Errorf("could not check staged conflict markers: %w", err)
	}
	output, err := i.run(ctx, "-C", directory, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// RecordCheck records success only if the exact tree and working copy stayed unchanged.
func (i GitInspector) RecordCheck(ctx context.Context, directory string, before string, command []string) error {
	after, err := i.ResolutionTree(ctx, directory)
	if err != nil {
		return err
	}
	if before != after || len(command) == 0 {
		return fmt.Errorf("tree changed while tests ran; review and rerun checks")
	}
	state, err := LoadResolution(directory)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(command)
	if err != nil {
		return err
	}
	// Arguments may contain credentials: retain an identity digest, not argv values.
	identity := []string{filepath.Base(command[0]), fmt.Sprintf("argv-sha256:%x", sha256.Sum256(encoded))}
	state.Tree, state.Check, state.Candidate = after, identity, ""
	return SaveResolution(directory, state)
}

func (i GitInspector) CheckedResolution(ctx context.Context, directory string) (ResolutionState, error) {
	state, err := LoadResolution(directory)
	if err != nil {
		return state, err
	}
	tree, err := i.ResolutionTree(ctx, directory)
	if err != nil {
		return state, err
	}
	if state.Tree != tree || len(state.Check) == 0 {
		return state, fmt.Errorf("no successful check for this exact tree; run resolve check first")
	}
	return state, nil
}

// PushResolution publishes only a new candidate, never an existing branch update.
func (i GitInspector) PushResolution(ctx context.Context, directory string, targetURL string, sourceURL string) (Candidate, error) {
	state, err := i.CheckedResolution(ctx, directory)
	if err != nil {
		return Candidate{}, err
	}
	if err := i.verifyRemoteHead(ctx, targetURL, state.Branch, state.TargetSHA); err != nil {
		return Candidate{}, err
	}
	if err := i.verifyRemoteHead(ctx, sourceURL, state.Branch, state.SourceSHA); err != nil {
		return Candidate{}, err
	}
	if state.Candidate == "" {
		date, err := i.mergeDate(ctx, directory, state.TargetSHA, state.SourceSHA)
		if err != nil {
			return Candidate{}, err
		}
		output, err := i.runWithEnv(ctx, []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}, "-C", directory,
			"commit-tree", state.Tree, "-p", state.TargetSHA, "-p", state.SourceSHA, "-m", "fix(sync): integrate reviewed "+state.Branch+" resolution")
		if err != nil {
			return Candidate{}, fmt.Errorf("create two-parent resolution commit (check Git identity): %w", err)
		}
		state.Candidate = strings.TrimSpace(string(output))
		if err := SaveResolution(directory, state); err != nil {
			return Candidate{}, err
		}
	}
	// Validate recovery data against the actual commit before any remote write.
	parents, err := i.run(ctx, "-C", directory, "show", "-s", "--format=%P %T", state.Candidate)
	if err != nil || strings.TrimSpace(string(parents)) != state.TargetSHA+" "+state.SourceSHA+" "+state.Tree {
		return Candidate{}, fmt.Errorf("candidate does not preserve the expected parents and checked tree")
	}
	name := "sync/resolved/" + state.Branch + "/" + state.Candidate
	ref := "refs/heads/" + name
	sha, exists, err := i.remoteRefSHA(ctx, targetURL, ref)
	if err != nil {
		return Candidate{}, err
	}
	if exists && sha != state.Candidate {
		return Candidate{}, fmt.Errorf("candidate branch already exists with different content; it was not changed")
	}
	candidate := Candidate{BranchName: name, CommitSHA: state.Candidate, AlreadyPushed: exists}
	if !exists {
		if err := i.pushNewResolutionRef(ctx, directory, targetURL, state.Candidate, ref); err != nil {
			return Candidate{}, fmt.Errorf("push new resolution candidate: %w", err)
		}
	}
	actual, exists, err := i.remoteRefSHA(ctx, targetURL, ref)
	if err != nil || !exists || actual != state.Candidate {
		return candidate, fmt.Errorf("cannot verify pushed candidate; no PR should be created")
	}
	if err := i.verifyRemoteHead(ctx, targetURL, state.Branch, state.TargetSHA); err != nil {
		return candidate, err
	}
	if err := i.verifyRemoteHead(ctx, sourceURL, state.Branch, state.SourceSHA); err != nil {
		return candidate, err
	}
	return candidate, nil
}

// Inspect the server-advertised old OID in a CLI-owned pre-push guard. This
// prevents even fast-forward updates if a ref appeared after ls-remote, without
// using any force option. Receive-pack then checks that same zero OID atomically.
func (i GitInspector) pushNewResolutionRef(ctx context.Context, directory string, targetURL string, sha string, ref string) error {
	root, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	hooks, err := os.MkdirTemp(filepath.Join(root, ".git"), "resolution-publish-hooks-")
	if err != nil {
		return err
	}
	hook := filepath.Join(hooks, "pre-push")
	defer func() {
		_ = os.Remove(hook)
		_ = os.Remove(hooks)
	}()
	guard := "#!/bin/sh\nwhile read -r local_ref local_oid remote_ref remote_oid; do\n  case \"$remote_oid\" in\n    *[!0]*) echo 'Ref appeared during publication; refusing any existing-ref update.' >&2; exit 1 ;;\n  esac\ndone\n"
	if err := os.WriteFile(hook, []byte(guard), 0o700); err != nil {
		return err
	}
	_, err = i.run(ctx, "-C", directory, "-c", "core.hooksPath="+hooks, "push", "--quiet", targetURL, sha+":"+ref)
	return err
}
