// Package repobinder manages the repo binding token stored in .git/aicg-binding.
package repobinder

import (
	"fmt"
	"os"
	"path/filepath"
)

const bindingFile = ".git/aicg-binding"

// BindingToken is a signed token that links a local repo to a GW-side repo_id.
type BindingToken struct {
	RepoID    string `json:"repo_id"`
	Token     string `json:"token"`
	MachineID string `json:"machine_id"`
}

// Load reads the binding token from .git/aicg-binding.
func Load(repoRoot string) (*BindingToken, error) {
	path := filepath.Join(repoRoot, bindingFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read binding token: %w", err)
	}
	// Simple key=value format; production would use JSON or JWT.
	t := &BindingToken{}
	for _, line := range splitLines(string(data)) {
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		parts := splitKeyVal(line, "=")
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "repo_id":
			t.RepoID = parts[1]
		case "token":
			t.Token = parts[1]
		case "machine_id":
			t.MachineID = parts[1]
		}
	}
	if t.RepoID == "" || t.Token == "" {
		return nil, fmt.Errorf("binding token incomplete: repo_id=%q token=%q", t.RepoID, t.Token)
	}
	return t, nil
}

// Save writes the binding token to .git/aicg-binding.
func Save(repoRoot string, t *BindingToken) error {
	path := filepath.Join(repoRoot, bindingFile)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create .git dir: %w", err)
	}
	content := fmt.Sprintf("repo_id=%s\ntoken=%s\nmachine_id=%s\n", t.RepoID, t.Token, t.MachineID)
	return os.WriteFile(path, []byte(content), 0644)
}

// FindRoot walks upward from cwd to find the nearest .git directory.
func FindRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := cwd; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && info.IsDir() {
			return dir, nil
		}
	}
	return "", fmt.Errorf("not in a git repository")
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func splitKeyVal(s, sep string) []string {
	for i := 0; i < len(s); i++ {
		if string(s[i]) == sep {
			return []string{s[:i], s[i+1:]}
		}
	}
	return []string{s}
}
