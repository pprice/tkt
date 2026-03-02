package engine

import (
	"fmt"
	"os"
	"path/filepath"
)

// JournalPath returns the path to the commits.jsonl for the given project.
func JournalPath(projectName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".tkt", "state", projectName, "commits.jsonl"), nil
}

// MutationLogPath returns the path to the mutations.jsonl for the given project.
func MutationLogPath(projectName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".tkt", "state", projectName, "mutations.jsonl"), nil
}

// CentralStoreRoot returns the central ticket store root directory.
// If TKT_ROOT is set, it is used (must be an absolute path).
// Otherwise defaults to ~/.tickets.
func CentralStoreRoot() (string, error) {
	if root := os.Getenv("TKT_ROOT"); root != "" {
		if !filepath.IsAbs(root) {
			return "", fmt.Errorf("TKT_ROOT must be an absolute path, got: %s", root)
		}
		return root, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".tickets"), nil
}

// CentralProjectDir returns ~/.tickets/<projectName>.
func CentralProjectDir(projectName string) (string, error) {
	root, err := CentralStoreRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, projectName), nil
}
