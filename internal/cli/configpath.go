package cli

import (
	"os"
	"path/filepath"
)

// configEnv names the manifest without a flag (useful when the command runs from anywhere).
const configEnv = "MULTI_SOURCE_REPO_SYNC_CONFIG"

// defaultConfigPath is where the manifest is looked for when --config is not given: the file the
// environment names, else config/repos.json next to where the command runs, else the one in the
// user's configuration directory. When none exists the first name is used, so the error says it.
func defaultConfigPath() string {
	if path := os.Getenv(configEnv); path != "" {
		return path
	}
	const local = "config/repos.json"
	if _, err := os.Stat(local); err == nil {
		return local
	}
	if dir, err := os.UserConfigDir(); err == nil {
		user := filepath.Join(dir, "multi-source-repo-sync", "repos.json")
		if _, err := os.Stat(user); err == nil {
			return user
		}
	}
	return local
}
