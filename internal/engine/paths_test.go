package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCentralStoreRootDefault(t *testing.T) {
	// Ensure TKT_ROOT is not set.
	t.Setenv("TKT_ROOT", "")

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	root, err := CentralStoreRoot()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".tickets")
	if root != want {
		t.Fatalf("expected %s, got %s", want, root)
	}
}

func TestCentralStoreRootFromEnv(t *testing.T) {
	custom := "/tmp/my-custom-tickets"
	t.Setenv("TKT_ROOT", custom)

	root, err := CentralStoreRoot()
	if err != nil {
		t.Fatal(err)
	}
	if root != custom {
		t.Fatalf("expected %s, got %s", custom, root)
	}
}

func TestCentralStoreRootRejectsRelativePath(t *testing.T) {
	t.Setenv("TKT_ROOT", "relative/path")

	_, err := CentralStoreRoot()
	if err == nil {
		t.Fatal("expected error for relative TKT_ROOT, got nil")
	}
	if got := err.Error(); got != "TKT_ROOT must be an absolute path, got: relative/path" {
		t.Fatalf("unexpected error message: %s", got)
	}
}

func TestCentralProjectDirRespectsEnv(t *testing.T) {
	custom := "/tmp/my-custom-tickets"
	t.Setenv("TKT_ROOT", custom)

	dir, err := CentralProjectDir("myproject")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(custom, "myproject")
	if dir != want {
		t.Fatalf("expected %s, got %s", want, dir)
	}
}
