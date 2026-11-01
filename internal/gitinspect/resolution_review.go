package gitinspect

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// ResolutionDiff avoids executing external diff/textconv programs from configuration.
func (i GitInspector) ResolutionDiff(ctx context.Context, directory string) (string, error) {
	if _, err := LoadResolution(directory); err != nil {
		return "", err
	}
	var output strings.Builder
	for _, staged := range []bool{true, false} {
		args := []string{"-C", directory, "diff", "--no-ext-diff", "--no-textconv", "--color=never"}
		if staged {
			output.WriteString("\n=== Mudanças no índice (inclui o merge automático) ===\n")
			args = append(args, "--cached")
		} else {
			output.WriteString("\n=== Mudanças no workspace / conflitos ===\n")
		}
		diff, err := i.run(ctx, args...)
		if err != nil {
			return "", err
		}
		output.Write(diff)
	}
	return output.String(), nil
}

func (i GitInspector) ResolutionUnstaged(ctx context.Context, directory string) ([]string, error) {
	if _, err := LoadResolution(directory); err != nil {
		return nil, err
	}
	output, err := i.run(ctx, "-C", directory, "ls-files", "--modified", "--deleted", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var paths []string
	for _, path := range strings.Split(string(output), "\x00") {
		if path != "" && !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// StageResolutionPath stages only the explicitly selected, still-pending path.
func (i GitInspector) StageResolutionPath(ctx context.Context, directory string, selected string) error {
	paths, err := i.ResolutionUnstaged(ctx, directory)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if path == selected {
			_, err := i.run(ctx, "--literal-pathspecs", "-C", directory, "add", "--", selected)
			return err
		}
	}
	return fmt.Errorf("selected path is no longer pending; inspect the workspace again")
}
