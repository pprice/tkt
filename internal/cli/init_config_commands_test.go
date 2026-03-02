package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/lawrips/tkt/internal/project"
)

func withWorkspaceNoTickets(t *testing.T, fn func(dir string)) {
	t.Helper()

	dir := t.TempDir()

	cwdMu.Lock()
	defer cwdMu.Unlock()

	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir temp: %v", err)
	}
	defer func() {
		if err := os.Chdir(original); err != nil {
			t.Fatalf("restore chdir: %v", err)
		}
	}()

	// Isolate HOME so tests never read the real user config.
	t.Setenv("HOME", t.TempDir())

	fn(dir)
}

func TestInitLocalWritesConfig(t *testing.T) {
	withWorkspaceNoTickets(t, func(dir string) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		out, _, err := runCmd(t, "", "init", "--project", "demo", "--store", "local", "--yes")
		if err != nil {
			t.Fatalf("init: %v", err)
		}
		if !strings.Contains(out, "Project registered in") {
			t.Fatalf("unexpected init output: %q", out)
		}

		cfg, err := project.Load()
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		entry, ok := cfg.Projects["demo"]
		if !ok {
			t.Fatalf("missing project demo in config")
		}
		if entry.Store != "local" {
			t.Fatalf("expected local store, got %s", entry.Store)
		}
		if !samePath(dir, entry.Path) {
			t.Fatalf("expected path %s, got %s", dir, entry.Path)
		}
		// No .git directory, so auto_link/auto_close default to false
		if entry.AutoLink || entry.AutoClose {
			t.Fatalf("expected false/false (no git repo), got %+v", entry)
		}
		if _, err := os.Stat(filepath.Join(dir, ".tickets")); err != nil {
			t.Fatalf("expected local .tickets directory: %v", err)
		}
	})
}

func TestInitUpgradePathCanKeepLocal(t *testing.T) {
	withWorkspace(t, func(dir string) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		if err := os.WriteFile(filepath.Join(dir, ".tickets", "a.md"), []byte("---\nid: a\n---\n# A\n"), 0644); err != nil {
			t.Fatalf("seed .tickets file: %v", err)
		}

		// Choice 2 (keep local). No auto-link/auto-close prompts (no git), no claude prompt.
		if _, _, err := runCmd(t, "2\n", "init", "--project", "demo"); err != nil {
			t.Fatalf("init upgrade keep local: %v", err)
		}

		cfg, err := project.Load()
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		entry := cfg.Projects["demo"]
		if entry.Store != "local" {
			t.Fatalf("expected local store after keep-local choice, got %s", entry.Store)
		}
		if _, err := os.Stat(filepath.Join(dir, ".tickets", "a.md")); err != nil {
			t.Fatalf("expected local ticket file to remain: %v", err)
		}
	})
}

func TestInitCentralCopiesTicketsAndKeepsBackup(t *testing.T) {
	withWorkspace(t, func(dir string) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		if err := os.WriteFile(filepath.Join(dir, ".tickets", "m.md"), []byte("---\nid: m\n---\n# M\n"), 0644); err != nil {
			t.Fatalf("seed .tickets file: %v", err)
		}

		out, _, err := runCmd(t, "", "init", "--project", "demo", "--store", "central", "--yes")
		if err != nil {
			t.Fatalf("init central: %v", err)
		}

		// Ticket should be copied to central store
		if _, err := os.Stat(filepath.Join(home, ".tickets", "demo", "m.md")); err != nil {
			t.Fatalf("expected ticket copied to central store: %v", err)
		}
		if _, err := os.Stat(filepath.Join(home, ".tickets", ".git")); err != nil {
			t.Fatalf("expected central monorepo git repo to be initialized: %v", err)
		}
		if commits := gitCommitCount(t, filepath.Join(home, ".tickets")); commits != 1 {
			t.Fatalf("expected one initial central-store commit after first init, got %d", commits)
		}

		// Original should still exist (copy, not move)
		if _, err := os.Stat(filepath.Join(dir, ".tickets", "m.md")); err != nil {
			t.Fatalf("expected local ticket to remain as backup: %v", err)
		}

		// Output should mention backup
		if !strings.Contains(out, "backup") {
			t.Fatalf("expected backup message in output, got: %q", out)
		}

		// Output should show setup complete status
		if !strings.Contains(out, "Setup complete") {
			t.Fatalf("expected setup complete message in output, got: %q", out)
		}

		// Second init should be idempotent (no extra commits)
		if _, _, err := runCmd(t, "", "init", "--project", "demo", "--store", "central", "--yes"); err != nil {
			t.Fatalf("second init central: %v", err)
		}
		if commits := gitCommitCount(t, filepath.Join(home, ".tickets")); commits != 1 {
			t.Fatalf("expected central-store init to remain idempotent, got %d commits", commits)
		}
	})
}

func TestInitSeedsWorkflowFileOnce(t *testing.T) {
	withWorkspaceNoTickets(t, func(_ string) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		if _, _, err := runCmd(t, "", "init", "--project", "demo", "--store", "local", "--yes"); err != nil {
			t.Fatalf("init: %v", err)
		}

		workflowPath := filepath.Join(home, ".tkt", "workflow.md")
		raw, err := os.ReadFile(workflowPath)
		if err != nil {
			t.Fatalf("read seeded workflow: %v", err)
		}
		if len(raw) == 0 {
			t.Fatalf("expected seeded workflow content")
		}

		custom := []byte("# Custom Workflow\n")
		if err := os.WriteFile(workflowPath, custom, 0644); err != nil {
			t.Fatalf("write custom workflow: %v", err)
		}

		if _, _, err := runCmd(t, "", "init", "--project", "demo", "--store", "local", "--yes"); err != nil {
			t.Fatalf("second init: %v", err)
		}

		raw, err = os.ReadFile(workflowPath)
		if err != nil {
			t.Fatalf("read workflow after second init: %v", err)
		}
		if string(raw) != string(custom) {
			t.Fatalf("expected init to preserve existing workflow, got %q", string(raw))
		}
	})
}

func TestConfigViewEditAndResolve(t *testing.T) {
	withWorkspaceNoTickets(t, func(dir string) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		if _, _, err := runCmd(t, "", "init", "--project", "demo", "--store", "local", "--yes"); err != nil {
			t.Fatalf("init: %v", err)
		}

		out, _, err := runCmd(t, "", "config")
		if err != nil {
			t.Fatalf("config show: %v", err)
		}
		if !strings.Contains(out, "demo") || !strings.Contains(out, "store: local") {
			t.Fatalf("unexpected config output: %q", out)
		}

		if _, _, err := runCmd(t, "", "config", "set", "demo", "auto_close", "false"); err != nil {
			t.Fatalf("config set explicit: %v", err)
		}
		if _, _, err := runCmd(t, "", "config", "set", "auto_link", "false"); err != nil {
			t.Fatalf("config set resolved project: %v", err)
		}

		cfg, err := project.Load()
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		entry := cfg.Projects["demo"]
		if entry.AutoClose || entry.AutoLink {
			t.Fatalf("expected false/false after config set, got %+v", entry)
		}
		if !samePath(dir, entry.Path) {
			t.Fatalf("expected project path %s, got %s", dir, entry.Path)
		}

		out, _, err = runCmd(t, "", "config", "resolve")
		if err != nil {
			t.Fatalf("config resolve: %v", err)
		}
		if !strings.Contains(out, "demo (config)") {
			t.Fatalf("unexpected resolve output: %q", out)
		}

		out, _, err = runCmd(t, "", "--project", "override", "config", "resolve")
		if err != nil {
			t.Fatalf("config resolve with --project override: %v", err)
		}
		if !strings.Contains(out, "override (flag)") {
			t.Fatalf("expected flag override precedence, got: %q", out)
		}
	})
}

func TestInitNoGitDefaultsAutoLinkOff(t *testing.T) {
	withWorkspaceNoTickets(t, func(dir string) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		_, stderr, err := runCmd(t, "", "init", "--project", "demo", "--store", "local", "--yes")
		if err != nil {
			t.Fatalf("init: %v", err)
		}

		// Should warn about missing git
		if !strings.Contains(stderr, "No git repository") {
			t.Fatalf("expected git warning on stderr, got: %q", stderr)
		}

		cfg, err := project.Load()
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		entry := cfg.Projects["demo"]
		if entry.AutoLink {
			t.Fatalf("expected auto_link=false without git, got true")
		}
		if entry.AutoClose {
			t.Fatalf("expected auto_close=false without git, got true")
		}
	})
}

func TestInitWithGitKeepsAutoLinkOn(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	withWorkspaceNoTickets(t, func(dir string) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		// Create a git repo with at least one commit
		exec.Command("git", "-C", dir, "init").Run()
		exec.Command("git", "-C", dir, "config", "user.email", "test@test.com").Run()
		exec.Command("git", "-C", dir, "config", "user.name", "test").Run()
		exec.Command("git", "-C", dir, "commit", "--allow-empty", "-m", "init").Run()

		out, _, err := runCmd(t, "", "init", "--project", "demo", "--store", "local", "--yes")
		if err != nil {
			t.Fatalf("init: %v", err)
		}

		// Should NOT warn about git
		if strings.Contains(out, "No git") {
			t.Fatalf("unexpected git warning in output: %q", out)
		}

		cfg, err := project.Load()
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		entry := cfg.Projects["demo"]
		if !entry.AutoLink {
			t.Fatalf("expected auto_link=true with git, got false")
		}
		if !entry.AutoClose {
			t.Fatalf("expected auto_close=true with git, got false")
		}
	})
}

func samePath(a, b string) bool {
	aAbs, errA := filepath.Abs(a)
	bAbs, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}

	aEval, errA := filepath.EvalSymlinks(aAbs)
	bEval, errB := filepath.EvalSymlinks(bAbs)
	if errA == nil && errB == nil {
		return filepath.Clean(aEval) == filepath.Clean(bEval)
	}
	return filepath.Clean(aAbs) == filepath.Clean(bAbs)
}

func TestInitCentralHonorsTktRoot(t *testing.T) {
	withWorkspaceNoTickets(t, func(dir string) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		customRoot := filepath.Join(t.TempDir(), "custom-store")
		t.Setenv("TKT_ROOT", customRoot)

		out, _, err := runCmd(t, "", "init", "--project", "demo", "--store", "central", "--yes")
		if err != nil {
			t.Fatalf("init central with TKT_ROOT: %v", err)
		}

		// Tickets should be in the custom root, not ~/.tickets.
		if _, err := os.Stat(filepath.Join(customRoot, "demo")); err != nil {
			t.Fatalf("expected project dir at custom TKT_ROOT: %v", err)
		}
		defaultRoot := filepath.Join(home, ".tickets", "demo")
		if _, err := os.Stat(defaultRoot); err == nil {
			t.Fatalf("expected NO project dir at default ~/.tickets when TKT_ROOT is set")
		}

		// Output should reference the custom path.
		if !strings.Contains(out, customRoot) {
			t.Fatalf("expected output to reference TKT_ROOT path %s, got: %q", customRoot, out)
		}
	})
}

func TestInitCentralRejectsRelativeTktRoot(t *testing.T) {
	withWorkspaceNoTickets(t, func(dir string) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("TKT_ROOT", "relative/path")

		_, _, err := runCmd(t, "", "init", "--project", "demo", "--store", "central", "--yes")
		if err == nil {
			t.Fatal("expected error for relative TKT_ROOT, got nil")
		}
		if !strings.Contains(err.Error(), "TKT_ROOT must be an absolute path") {
			t.Fatalf("expected absolute path error, got: %v", err)
		}
	})
}

func gitCommitCount(t *testing.T, repo string) int {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "rev-list", "--count", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-list --count HEAD failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parse commit count: %v output=%q", err, strings.TrimSpace(string(out)))
	}
	return n
}
