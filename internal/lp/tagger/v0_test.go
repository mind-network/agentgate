package tagger

import (
	"testing"
)

func TestClassifyCodeEdit(t *testing.T) {
	r := Classify([]string{"src/main.go", "pkg/utils.go"}, "go", 120, 30, false, false)
	if r.TaskType != "code_edit" {
		t.Errorf("expected code_edit, got %s", r.TaskType)
	}
}

func TestClassifyTestOutput(t *testing.T) {
	r := Classify([]string{"src/main_test.go"}, "go", 10, 5, true, false)
	if r.TaskType != "test_output" {
		t.Errorf("expected test_output, got %s", r.TaskType)
	}
}

func TestClassifySummary(t *testing.T) {
	r := Classify([]string{"docs/readme.md", "CHANGELOG.md"}, "", 50, 10, false, false)
	if r.TaskType != "summary" {
		t.Errorf("expected summary, got %s", r.TaskType)
	}
}

func TestClassifyUnknown(t *testing.T) {
	r := Classify(nil, "", 0, 0, false, false)
	if r.TaskType != "unknown" {
		t.Errorf("expected unknown, got %s", r.TaskType)
	}
}

func TestClassifyComplexity(t *testing.T) {
	r := Classify([]string{"src/big.go"}, "go", 600, 100, false, false)
	if r.Complexity != "high" {
		t.Errorf("expected high complexity (700 total), got %s", r.Complexity)
	}
}
