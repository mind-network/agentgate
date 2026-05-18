// Package tagger provides heuristic task type classification for LP-side hints.
package tagger

import (
	"strings"
)

// Result is the output of the v0 tagger.
type Result struct {
	TaskType        string   // planning, architecture, repo_search, file_reading, simple_edit, code_edit, test_output, debug, review, security_review, summary, unknown
	Complexity      string   // low, medium, high, unknown
	Language        string   // detected primary language
	AgenticLoop     bool     // whether tool_result suggests agentic continuation
}

// Classify produces a heuristic classification from file paths and diff summary.
// This is a v0 heuristic; the server reclassifier always recomputes independently.
func Classify(filePaths []string, language string, linesAdded, linesRemoved int, containsTestFailure, containsStackTrace bool) *Result {
	r := &Result{
		Language:   language,
		Complexity: "unknown",
		TaskType:   "unknown",
	}

	r.TaskType = classifyTaskType(filePaths, linesAdded, linesRemoved, containsTestFailure, containsStackTrace)
	r.Complexity = classifyComplexity(filePaths, linesAdded, linesRemoved)
	return r
}

func classifyTaskType(filePaths []string, added, removed int, testFail, stackTrace bool) string {
	if testFail || stackTrace {
		return "test_output"
	}
	// Heuristic: file path patterns
	hasConfig := false
	hasDoc := false
	hasTest := false
	hasSrc := false

	for _, p := range filePaths {
		switch {
		case strings.HasSuffix(p, "_test.go") || strings.HasSuffix(p, "_test.py") || strings.HasSuffix(p, ".test.ts"):
			hasTest = true
		case strings.HasSuffix(p, ".md") || strings.HasSuffix(p, ".rst") || strings.HasPrefix(p, "docs/"):
			hasDoc = true
		case strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml") || strings.HasSuffix(p, ".toml") ||
			strings.HasSuffix(p, ".json") || strings.Contains(p, "Dockerfile") || strings.Contains(p, "Makefile"):
			hasConfig = true
		default:
			hasSrc = true
		}
	}

	if hasDoc && !hasSrc && !hasTest {
		return "summary"
	}
	if hasConfig && !hasSrc {
		return "simple_edit"
	}
	if hasTest {
		return "test_output"
	}
	if added > 200 || removed > 200 {
		return "code_edit"
	}
	if added > 50 || removed > 50 {
		return "code_edit"
	}
	if hasSrc {
		return "code_edit"
	}
	return "unknown"
}

func classifyComplexity(filePaths []string, added, removed int) string {
	total := added + removed
	switch {
	case total > 500:
		return "high"
	case total > 100:
		return "medium"
	case total > 0:
		return "low"
	default:
		return "unknown"
	}
}
