package classifier

import (
	"testing"
)

func TestReclassifyCodeEdit(t *testing.T) {
	r := Reclassify([]string{"src/main.go", "pkg/handler.go"}, "go", 150, 50, false, false)
	if r.TaskType != "code_edit" {
		t.Errorf("expected code_edit, got %s", r.TaskType)
	}
}

func TestReclassifyDebug(t *testing.T) {
	r := Reclassify([]string{"src/main.go"}, "go", 10, 5, true, true)
	if r.TaskType != "debug" {
		t.Errorf("expected debug, got %s", r.TaskType)
	}
}

func TestReclassifySummary(t *testing.T) {
	r := Reclassify([]string{"README.md"}, "", 50, 10, false, false)
	if r.TaskType != "summary" {
		t.Errorf("expected summary, got %s", r.TaskType)
	}
}

func TestReclassifyArchitecture(t *testing.T) {
	r := Reclassify([]string{"docs/architecture/SYSTEM-DESIGN.md"}, "", 100, 50, false, false)
	if r.TaskType != "architecture" {
		t.Errorf("expected architecture, got %s", r.TaskType)
	}
}

func TestReclassifySimpleEdit(t *testing.T) {
	r := Reclassify([]string{".github/workflows/ci.yaml"}, "", 20, 5, false, false)
	if r.TaskType != "simple_edit" {
		t.Errorf("expected simple_edit, got %s", r.TaskType)
	}
}

func TestReclassifyHighSensitivity(t *testing.T) {
	r := Reclassify([]string{"internal/auth/credentials.go"}, "go", 5, 2, false, false)
	if r.DataSensitivity != "high" {
		t.Errorf("expected high sensitivity, got %s", r.DataSensitivity)
	}
}

func TestReclassifyComplexityHigh(t *testing.T) {
	paths := make([]string, 25)
	for i := range paths {
		paths[i] = "src/file.go"
	}
	r := Reclassify(paths, "go", 600, 500, false, false)
	if r.Complexity != "high" {
		t.Errorf("expected high complexity, got %s", r.Complexity)
	}
}
