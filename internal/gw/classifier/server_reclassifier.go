// Package classifier provides the server-side reclassifier that recomputes
// task_type / complexity / data_sensitivity independently of client hints.
package classifier

import (
	"strings"
)

// Result is the output of the server reclassifier.
type Result struct {
	TaskType        string   `json:"task_type"`
	Complexity      string   `json:"complexity"`
	DataSensitivity string   `json:"data_sensitivity"`
	Language        string   `json:"language"`
	HasSecretFinding bool    `json:"has_secret_finding"`
	Reasons         []string `json:"reasons"`
}

// Reclassify runs the lite heuristic reclassifier.
// Client hint values are used as priors but always recomputed independently.
func Reclassify(filePaths []string, primaryLanguage string, linesAdded, linesRemoved int, containsTestFailure, containsStackTrace bool) *Result {
	r := &Result{
		Language:        primaryLanguage,
		TaskType:        "unknown",
		Complexity:      "unknown",
		DataSensitivity: "unknown",
	}

	r.TaskType = reclassifyTaskType(filePaths, linesAdded, linesRemoved, containsTestFailure, containsStackTrace)
	r.Complexity = reclassifyComplexity(filePaths, linesAdded, linesRemoved)
	r.DataSensitivity = reclassifySensitivity(filePaths)
	return r
}

func reclassifyTaskType(filePaths []string, added, removed int, testFail, stackTrace bool) string {
	// test_output / debug: high priority signals
	if testFail || stackTrace {
		return "debug"
	}

	hasDoc := false
	hasConfig := false
	hasSrc := false
	hasTest := false
	hasArch := false

	for _, p := range filePaths {
		switch {
		case strings.HasPrefix(p, "docs/architecture/") || strings.HasPrefix(p, "docs/design/"):
			hasArch = true
		case strings.HasSuffix(p, "_test.go") || strings.HasSuffix(p, "_test.py") || strings.Contains(p, "/test/"):
			hasTest = true
		case strings.HasSuffix(p, ".md") || strings.HasSuffix(p, ".rst"):
			hasDoc = true
		case strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml") || strings.HasSuffix(p, ".toml") ||
			strings.HasSuffix(p, ".json") || strings.Contains(p, "Dockerfile") || strings.Contains(p, "Makefile"):
			hasConfig = true
		default:
			hasSrc = true
		}
	}

	// Architecture / design docs → architecture
	if hasArch && !hasSrc && !hasTest {
		return "architecture"
	}
	// Summary: docs-only changes
	if hasDoc && !hasSrc && !hasTest && !hasConfig {
		return "summary"
	}
	// Simple edit: config-only changes
	if hasConfig && !hasSrc && !hasTest {
		return "simple_edit"
	}
	// Test output
	if hasTest || testFail {
		return "test_output"
	}
	// Code edit: source files changed
	if hasSrc {
		if added > 100 || removed > 100 {
			return "code_edit"
		}
		return "simple_edit"
	}
	// File reading: no changes at all
	if added == 0 && removed == 0 {
		return "file_reading"
	}
	return "unknown"
}

func reclassifyComplexity(filePaths []string, added, removed int) string {
	nFiles := len(filePaths)
	total := added + removed

	// Complexity heuristic
	if nFiles > 20 || total > 1000 {
		return "high"
	}
	if nFiles > 5 || total > 200 {
		return "medium"
	}
	if total > 0 {
		return "low"
	}
	return "unknown"
}

func reclassifySensitivity(filePaths []string) string {
	for _, p := range filePaths {
		lower := strings.ToLower(p)
		switch {
		case strings.Contains(lower, "secret") || strings.Contains(lower, "key") ||
			strings.Contains(lower, "credential") || strings.Contains(lower, "token"):
			return "high"
		case strings.Contains(lower, "config") || strings.Contains(lower, "auth") ||
			strings.Contains(lower, "payment") || strings.Contains(lower, "billing"):
			return "medium"
		}
	}
	return "low"
}
