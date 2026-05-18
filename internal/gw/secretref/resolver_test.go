package secretref

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveEnv(t *testing.T) {
	if err := os.Setenv("AG_TEST_SECRET", "sk-test-123"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Unsetenv("AG_TEST_SECRET") }()

	val, err := Resolve("env://AG_TEST_SECRET")
	if err != nil {
		t.Fatal(err)
	}
	if val != "sk-test-123" {
		t.Errorf("expected sk-test-123, got %q", val)
	}
}

func TestResolveEnvMissing(t *testing.T) {
	_, err := Resolve("env://NONEXISTENT_VAR_XYZ123")
	if err == nil {
		t.Fatal("expected error for missing env var")
	}
	t.Logf("got expected error: %v", err)
}

func TestResolveEnvEmptyName(t *testing.T) {
	_, err := Resolve("env://")
	if err == nil {
		t.Fatal("expected error for empty env var name")
	}
}

func TestResolveFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.txt")
	if err := os.WriteFile(path, []byte("my-secret-key\n"), 0600); err != nil {
		t.Fatal(err)
	}

	val, err := Resolve("file://" + path)
	if err != nil {
		t.Fatal(err)
	}
	if val != "my-secret-key" {
		t.Errorf("expected my-secret-key, got %q", val)
	}
}

func TestResolveFileRelativePath(t *testing.T) {
	_, err := Resolve("file://relative/path/key.txt")
	if err == nil {
		t.Fatal("expected error for relative path")
	}
	t.Logf("got expected error: %v", err)
}

func TestResolveFileDotDot(t *testing.T) {
	_, err := Resolve("file:///etc/../passwd")
	if err == nil {
		t.Fatal("expected error for path with ..")
	}
	t.Logf("got expected error: %v", err)
}

func TestResolveFileMissing(t *testing.T) {
	_, err := Resolve("file:///nonexistent/path/key.txt")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	t.Logf("got expected error: %v", err)
}

func TestResolveVault(t *testing.T) {
	_, err := Resolve("vault://secret/api-key")
	if err == nil {
		t.Fatal("expected error for vault://")
	}
	if err.Error() == "" || len(err.Error()) < 10 {
		t.Error("expected descriptive error message")
	}
	t.Logf("got expected error: %v", err)
}

func TestResolveEmpty(t *testing.T) {
	val, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if val != "" {
		t.Errorf("expected empty string, got %q", val)
	}
}

func TestResolveUnsupportedScheme(t *testing.T) {
	_, err := Resolve("s3://bucket/key")
	if err == nil {
		t.Fatal("expected error for unsupported scheme")
	}
	t.Logf("got expected error: %v", err)
}
