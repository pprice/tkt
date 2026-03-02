package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lawrips/tkt/internal/project"
	"github.com/lawrips/tkt/internal/ticket"
)

func TestWatchOnceAppendsJournalWithDedupeAndCatchup(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	withWorkspaceNoTickets(t, func(dir string) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		runGit := func(args ...string) {
			cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
			}
		}
		commitWithDate := func(name, message, date string) {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte(name+"\n"), 0644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
			runGit("add", name)
			cmd := exec.Command("git", "-C", dir, "commit", "-m", message)
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_DATE="+date,
				"GIT_COMMITTER_DATE="+date,
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git commit failed: %v\n%s", err, string(out))
			}
		}

		runGit("init")
		runGit("config", "user.email", "tkt@example.com")
		runGit("config", "user.name", "tkt")

		// Use fixed timestamps instead of time.Sleep to avoid flakiness.
		oldDate := time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339)
		registration := time.Now().UTC().Add(-5 * time.Second).Format(time.RFC3339)
		newDate := time.Now().UTC().Format(time.RFC3339)

		// Commit before registration timestamp; should be ignored on first run.
		commitWithDate("old.txt", "[old-1] before registration", oldDate)
		commitWithDate("new.txt", "[new-1] first watched commit", newDate)

		cfg := project.Config{
			Projects: map[string]project.ProjectConfig{
				"demo": {
					Path:         project.DetectProjectPath(dir),
					Store:        "local",
					AutoLink:     true,
					AutoClose:    true,
					RegisteredAt: registration,
				},
			},
		}
		if err := project.Save(cfg); err != nil {
			t.Fatalf("save config: %v", err)
		}

		if _, _, err := runCmd(t, "", "watch", "--once"); err != nil {
			t.Fatalf("first watch --once: %v", err)
		}

		journalPath := filepath.Join(home, ".tkt", "state", "demo", "commits.jsonl")
		entries := readJournalEntriesFromFile(t, journalPath)
		if len(entries) != 1 {
			t.Fatalf("expected 1 entry after first watch, got %d (%+v)", len(entries), entries)
		}
		if entries[0]["ticket"] != "new-1" {
			t.Fatalf("expected new-1 entry, got %+v", entries[0])
		}
		// Watcher should populate diff stat fields.
		filesRaw, ok := entries[0]["files_changed"].([]any)
		if !ok || len(filesRaw) != 1 {
			t.Fatalf("expected 1 file in files_changed, got %v", entries[0]["files_changed"])
		}
		foundNew := false
		for _, file := range filesRaw {
			if name, _ := file.(string); name == "new.txt" {
				foundNew = true
				break
			}
		}
		if !foundNew {
			t.Fatalf("expected files_changed to contain new.txt, got %v", filesRaw)
		}
		if added, _ := entries[0]["lines_added"].(float64); added != 1 {
			t.Fatalf("expected lines_added=1, got %v", entries[0]["lines_added"])
		}
		if removedRaw, ok := entries[0]["lines_removed"]; ok {
			if removed, _ := removedRaw.(float64); removed != 0 {
				t.Fatalf("expected lines_removed=0, got %v", removedRaw)
			}
		}
		branch, _ := entries[0]["branch"].(string)
		if strings.TrimSpace(branch) == "" {
			t.Fatalf("expected non-empty branch, got %v", entries[0]["branch"])
		}

		// Re-run without new commits should dedupe by SHA and keep same count.
		if _, _, err := runCmd(t, "", "watch", "--once"); err != nil {
			t.Fatalf("second watch --once: %v", err)
		}
		entries = readJournalEntriesFromFile(t, journalPath)
		if len(entries) != 1 {
			t.Fatalf("expected deduped journal count 1, got %d", len(entries))
		}

		// New commit after restart should be caught up and appended.
		catchupDate := time.Now().UTC().Add(time.Second).Format(time.RFC3339)
		commitWithDate("close.txt", "fixes [new-2] follow-up", catchupDate)
		if _, _, err := runCmd(t, "", "watch", "--once"); err != nil {
			t.Fatalf("third watch --once: %v", err)
		}
		entries = readJournalEntriesFromFile(t, journalPath)
		if len(entries) != 2 {
			t.Fatalf("expected 2 entries after catchup, got %d (%+v)", len(entries), entries)
		}

		hasClose := false
		for _, row := range entries {
			ticketID, _ := row["ticket"].(string)
			action, _ := row["action"].(string)
			if ticketID == "new-2" && action == "close" {
				hasClose = true
			}
		}
		if !hasClose {
			t.Fatalf("expected close action for new-2, entries=%+v", entries)
		}
	})
}

func readJournalEntriesFromFile(t *testing.T, path string) []map[string]any {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	defer f.Close()

	out := make([]map[string]any, 0)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("invalid journal line %q: %v", line, err)
		}
		out = append(out, row)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan journal: %v", err)
	}
	return out
}

func TestWatchAutoCloseUpdatesTicketStatus(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	withWorkspace(t, func(dir string) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		runGit := func(args ...string) {
			cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
			}
		}
		runGit("init")
		runGit("config", "user.email", "tkt@example.com")
		runGit("config", "user.name", "tkt")

		seedTicket(t, "abc-1", ticket.Frontmatter{
			Status:   "open",
			Type:     "task",
			Priority: 1,
			Created:  time.Now().UTC().Format(time.RFC3339),
		}, ticket.Body{Title: "Close me"})

		if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x\n"), 0644); err != nil {
			t.Fatalf("write x: %v", err)
		}
		runGit("add", "x.txt")
		runGit("commit", "-m", "fixes [abc-1] done")

		cfg := project.Config{
			Projects: map[string]project.ProjectConfig{
				"demo": {
					Path:         project.DetectProjectPath(dir),
					Store:        "local",
					AutoLink:     true,
					AutoClose:    true,
					RegisteredAt: time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339),
				},
			},
		}
		if err := project.Save(cfg); err != nil {
			t.Fatalf("save config: %v", err)
		}

		if _, _, err := runCmd(t, "", "watch", "--once"); err != nil {
			t.Fatalf("watch --once: %v", err)
		}

		record, err := ticket.LoadByID(ticket.DefaultDir, "abc-1")
		if err != nil {
			t.Fatalf("load ticket abc-1: %v", err)
		}
		if record.Front.Status != "closed" {
			t.Fatalf("expected abc-1 to be auto-closed, got %s", record.Front.Status)
		}
	})
}

func TestWatchMatchesCloseDirectiveInCommitBody(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	withWorkspace(t, func(dir string) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		runGit := func(args ...string) {
			cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
			}
		}
		runGit("init")
		runGit("config", "user.email", "tkt@example.com")
		runGit("config", "user.name", "tkt")

		seedTicket(t, "body-1", ticket.Frontmatter{
			Status:   "open",
			Type:     "task",
			Priority: 1,
			Created:  time.Now().UTC().Format(time.RFC3339),
		}, ticket.Body{Title: "Close from body"})

		if err := os.WriteFile(filepath.Join(dir, "body.txt"), []byte("body\n"), 0644); err != nil {
			t.Fatalf("write body.txt: %v", err)
		}
		runGit("add", "body.txt")
		runGit("commit", "-m", "watcher parse body", "-m", "closes [body-1]")

		cfg := project.Config{
			Projects: map[string]project.ProjectConfig{
				"demo": {
					Path:         project.DetectProjectPath(dir),
					Store:        "local",
					AutoLink:     true,
					AutoClose:    true,
					RegisteredAt: time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339),
				},
			},
		}
		if err := project.Save(cfg); err != nil {
			t.Fatalf("save config: %v", err)
		}

		if _, _, err := runCmd(t, "", "watch", "--once"); err != nil {
			t.Fatalf("watch --once: %v", err)
		}

		record, err := ticket.LoadByID(ticket.DefaultDir, "body-1")
		if err != nil {
			t.Fatalf("load ticket body-1: %v", err)
		}
		if record.Front.Status != "closed" {
			t.Fatalf("expected body-1 to be auto-closed, got %s", record.Front.Status)
		}
	})
}

func TestWatchCentralStoreAutoCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	withWorkspaceNoTickets(t, func(dir string) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		runGit := func(repo string, args ...string) {
			cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git -C %s %v failed: %v\n%s", repo, args, err, string(out))
			}
		}

		runGit(dir, "init")
		runGit(dir, "config", "user.email", "tkt@example.com")
		runGit(dir, "config", "user.name", "tkt")

		centralRoot := filepath.Join(home, ".tickets")
		centralDir := filepath.Join(centralRoot, "demo")
		if err := os.MkdirAll(centralDir, 0755); err != nil {
			t.Fatalf("mkdir central dir: %v", err)
		}
		runGit(centralRoot, "init")
		runGit(centralRoot, "config", "user.email", "tkt@example.com")
		runGit(centralRoot, "config", "user.name", "tkt")

		centralTicketPath := filepath.Join(centralDir, "central-1.md")
		if err := os.WriteFile(centralTicketPath, []byte("---\nid: central-1\nstatus: open\ndeps: []\nlinks: []\ncreated: 2026-02-25T00:00:00Z\ntype: task\npriority: 1\n---\n# Central\n"), 0644); err != nil {
			t.Fatalf("write central ticket: %v", err)
		}
		runGit(centralRoot, "add", "-A")
		runGit(centralRoot, "commit", "-m", "seed central ticket")

		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0644); err != nil {
			t.Fatalf("write a.txt: %v", err)
		}
		runGit(dir, "add", "a.txt")
		runGit(dir, "commit", "-m", "fixes [central-1] close central ticket")

		cfg := project.Config{
			Projects: map[string]project.ProjectConfig{
				"demo": {
					Path:         project.DetectProjectPath(dir),
					Store:        "central",
					AutoLink:     true,
					AutoClose:    true,
					RegisteredAt: time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339),
				},
			},
		}
		if err := project.Save(cfg); err != nil {
			t.Fatalf("save config: %v", err)
		}

		if _, _, err := runCmd(t, "", "watch", "--once"); err != nil {
			t.Fatalf("watch --once central: %v", err)
		}

		record, err := ticket.LoadRecord(centralTicketPath)
		if err != nil {
			t.Fatalf("load central ticket: %v", err)
		}
		if record.Front.Status != "closed" {
			t.Fatalf("expected central ticket closed, got %s", record.Front.Status)
		}

		logOut, err := exec.Command("git", "-C", centralRoot, "log", "--pretty=%s", "-n", "1").Output()
		if err != nil {
			t.Fatalf("read central git log: %v", err)
		}
		if !strings.Contains(string(logOut), "tkt: sync") {
			t.Fatalf("expected central sync commit message, got %q", string(logOut))
		}
	})
}

func TestWatchCentralStoreCommitsCLIMutations(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	withWorkspaceNoTickets(t, func(dir string) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		runGit := func(repo string, args ...string) {
			cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git -C %s %v failed: %v\n%s", repo, args, err, string(out))
			}
		}

		assertClean := func(label string) {
			t.Helper()
			out, err := exec.Command("git", "-C", filepath.Join(home, ".tickets"), "status", "--porcelain").Output()
			if err != nil {
				t.Fatalf("git status after %s: %v", label, err)
			}
			if strings.TrimSpace(string(out)) != "" {
				t.Fatalf("expected clean central tree after %s, got %q", label, string(out))
			}
		}

		// Project repo with initial commit.
		runGit(dir, "init")
		runGit(dir, "config", "user.email", "tkt@example.com")
		runGit(dir, "config", "user.name", "tkt")
		if err := os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init\n"), 0644); err != nil {
			t.Fatalf("write init: %v", err)
		}
		runGit(dir, "add", "init.txt")
		runGit(dir, "commit", "-m", "initial")

		// Central store with initial commit.
		centralRoot := filepath.Join(home, ".tickets")
		centralDir := filepath.Join(centralRoot, "demo")
		if err := os.MkdirAll(centralDir, 0755); err != nil {
			t.Fatalf("mkdir central dir: %v", err)
		}
		runGit(centralRoot, "init")
		runGit(centralRoot, "config", "user.email", "tkt@example.com")
		runGit(centralRoot, "config", "user.name", "tkt")
		if err := os.WriteFile(filepath.Join(centralRoot, ".gitkeep"), []byte(""), 0644); err != nil {
			t.Fatalf("write gitkeep: %v", err)
		}
		runGit(centralRoot, "add", "-A")
		runGit(centralRoot, "commit", "-m", "init central store")

		cfg := project.Config{
			Projects: map[string]project.ProjectConfig{
				"demo": {
					Path:         project.DetectProjectPath(dir),
					Store:        "central",
					AutoLink:     true,
					AutoClose:    true,
					RegisteredAt: time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339),
				},
			},
		}
		if err := project.Save(cfg); err != nil {
			t.Fatalf("save config: %v", err)
		}

		// Create ticket via CLI.
		if _, _, err := runCmd(t, "", "create", "CLI mutation test", "--id", "mut-1"); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := os.Stat(filepath.Join(centralDir, "mut-1.md")); err != nil {
			t.Fatalf("expected mut-1.md on disk: %v", err)
		}
		if _, _, err := runCmd(t, "", "watch", "--once"); err != nil {
			t.Fatalf("watch --once after create: %v", err)
		}
		assertClean("create")

		// Edit ticket.
		if _, _, err := runCmd(t, "", "edit", "mut-1", "-s", "in_progress"); err != nil {
			t.Fatalf("edit: %v", err)
		}
		if _, _, err := runCmd(t, "", "watch", "--once"); err != nil {
			t.Fatalf("watch --once after edit: %v", err)
		}
		assertClean("edit")

		// Add note.
		if _, _, err := runCmd(t, "", "add-note", "mut-1", "This is a note"); err != nil {
			t.Fatalf("add-note: %v", err)
		}
		if _, _, err := runCmd(t, "", "watch", "--once"); err != nil {
			t.Fatalf("watch --once after add-note: %v", err)
		}
		assertClean("add-note")

		// Delete ticket.
		if _, _, err := runCmd(t, "", "delete", "mut-1"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, _, err := runCmd(t, "", "watch", "--once"); err != nil {
			t.Fatalf("watch --once after delete: %v", err)
		}
		assertClean("delete")

		// Verify total commits: init + create + edit + add-note + delete = 5.
		countOut, err := exec.Command("git", "-C", centralRoot, "rev-list", "--count", "HEAD").Output()
		if err != nil {
			t.Fatalf("rev-list count: %v", err)
		}
		if count := strings.TrimSpace(string(countOut)); count != "5" {
			t.Fatalf("expected 5 central commits, got %s", count)
		}
	})
}

func TestWatchCentralStoreCommitsWithAutoFeaturesDisabled(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	withWorkspaceNoTickets(t, func(dir string) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		runGit := func(repo string, args ...string) {
			cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git -C %s %v failed: %v\n%s", repo, args, err, string(out))
			}
		}

		runGit(dir, "init")
		runGit(dir, "config", "user.email", "tkt@example.com")
		runGit(dir, "config", "user.name", "tkt")
		if err := os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init\n"), 0644); err != nil {
			t.Fatalf("write init: %v", err)
		}
		runGit(dir, "add", "init.txt")
		runGit(dir, "commit", "-m", "initial")

		centralRoot := filepath.Join(home, ".tickets")
		centralDir := filepath.Join(centralRoot, "demo")
		if err := os.MkdirAll(centralDir, 0755); err != nil {
			t.Fatalf("mkdir central dir: %v", err)
		}
		runGit(centralRoot, "init")
		runGit(centralRoot, "config", "user.email", "tkt@example.com")
		runGit(centralRoot, "config", "user.name", "tkt")
		if err := os.WriteFile(filepath.Join(centralRoot, ".gitkeep"), []byte(""), 0644); err != nil {
			t.Fatalf("write gitkeep: %v", err)
		}
		runGit(centralRoot, "add", "-A")
		runGit(centralRoot, "commit", "-m", "init central store")

		cfg := project.Config{
			Projects: map[string]project.ProjectConfig{
				"demo": {
					Path:         project.DetectProjectPath(dir),
					Store:        "central",
					AutoLink:     false,
					AutoClose:    false,
					RegisteredAt: time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339),
				},
			},
		}
		if err := project.Save(cfg); err != nil {
			t.Fatalf("save config: %v", err)
		}

		// Create ticket with auto-link and auto-close both disabled.
		if _, _, err := runCmd(t, "", "create", "No auto features", "--id", "noauto-1"); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, _, err := runCmd(t, "", "watch", "--once"); err != nil {
			t.Fatalf("watch --once: %v", err)
		}

		// Central store should still have committed the new ticket.
		out, err := exec.Command("git", "-C", centralRoot, "status", "--porcelain").Output()
		if err != nil {
			t.Fatalf("git status: %v", err)
		}
		if strings.TrimSpace(string(out)) != "" {
			t.Fatalf("expected clean central tree, got %q", string(out))
		}

		countOut, err := exec.Command("git", "-C", centralRoot, "rev-list", "--count", "HEAD").Output()
		if err != nil {
			t.Fatalf("rev-list count: %v", err)
		}
		if count := strings.TrimSpace(string(countOut)); count != "2" {
			t.Fatalf("expected 2 central commits (init + create), got %s", count)
		}
	})
}

func TestExtractTicketActionsCloseSyntaxVariants(t *testing.T) {
	cases := []struct {
		name    string
		message string
		ticket  string
	}{
		{name: "space", message: "Closes [v-1]", ticket: "v-1"},
		{name: "no-space", message: "Closes[v-2]", ticket: "v-2"},
		{name: "colon-space", message: "CLoses: [v-3]", ticket: "v-3"},
		{name: "colon-no-space", message: "fixes:[v-4]", ticket: "v-4"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			actions := extractTicketActions(tc.message)
			if got := actions[tc.ticket]; got != "close" {
				t.Fatalf("expected %s to be close, got %q (actions=%v)", tc.ticket, got, actions)
			}
		})
	}
}

func TestExtractTicketActionsRefOnly(t *testing.T) {
	actions := extractTicketActions("[my-ticket] some work done")
	if got := actions["my-ticket"]; got != "ref" {
		t.Fatalf("expected ref, got %q (actions=%v)", got, actions)
	}
}

func TestExtractTicketActionsMixedCloseAndRef(t *testing.T) {
	actions := extractTicketActions("Closes: [close-me] also refs [ref-me]")

	if got := actions["close-me"]; got != "close" {
		t.Fatalf("expected close-me=close, got %q", got)
	}
	if got := actions["ref-me"]; got != "ref" {
		t.Fatalf("expected ref-me=ref, got %q", got)
	}
}

func TestExtractTicketActionsCloseWinsOverRef(t *testing.T) {
	// "Closes [x]" also matches the ref pattern "[x]". Close should win.
	actions := extractTicketActions("Closes [dual-1]")
	if got := actions["dual-1"]; got != "close" {
		t.Fatalf("expected close to win over ref for dual-1, got %q", got)
	}
}

func TestExtractTicketActionsMultipleRefs(t *testing.T) {
	actions := extractTicketActions("[a-1] and [b-2] both referenced")
	if len(actions) != 2 {
		t.Fatalf("expected 2 actions, got %d: %v", len(actions), actions)
	}
	if actions["a-1"] != "ref" || actions["b-2"] != "ref" {
		t.Fatalf("expected both ref, got %v", actions)
	}
}

func TestWatchSetsWorkDurationForLiveCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	withWorkspaceNoTickets(t, func(dir string) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		runGit := func(args ...string) {
			cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
			}
		}
		runGitOutput := func(args ...string) string {
			cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("git %v failed: %v", args, err)
			}
			return strings.TrimSpace(string(out))
		}
		commitWithDate := func(name, message, date string) {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte(name+"\n"), 0644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
			runGit("add", name)
			cmd := exec.Command("git", "-C", dir, "commit", "-m", message)
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_DATE="+date,
				"GIT_COMMITTER_DATE="+date,
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git commit failed: %v\n%s", err, string(out))
			}
		}

		runGit("init")
		runGit("config", "user.email", "tkt@example.com")
		runGit("config", "user.name", "tkt")

		parentDate := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339)
		liveDate := time.Now().UTC().Format(time.RFC3339)
		commitWithDate("base.txt", "base", parentDate)
		commitWithDate("live.txt", "[dur-live-1] live commit", liveDate)

		cfg := project.Config{
			Projects: map[string]project.ProjectConfig{
				"demo": {
					Path:         project.DetectProjectPath(dir),
					Store:        "local",
					AutoLink:     true,
					AutoClose:    true,
					RegisteredAt: time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339),
				},
			},
		}
		if err := project.Save(cfg); err != nil {
			t.Fatalf("save config: %v", err)
		}

		if _, _, err := runCmd(t, "", "watch", "--once"); err != nil {
			t.Fatalf("watch --once: %v", err)
		}

		journalPath := filepath.Join(home, ".tkt", "state", "demo", "commits.jsonl")
		entries := readJournalEntriesFromFile(t, journalPath)
		if len(entries) != 1 {
			t.Fatalf("expected 1 entry, got %d (%+v)", len(entries), entries)
		}

		row := entries[0]
		if got, _ := row["ticket"].(string); got != "dur-live-1" {
			t.Fatalf("expected ticket dur-live-1, got %q", got)
		}

		expectedStart := runGitOutput("log", "-1", "--pretty=format:%aI", "HEAD^")
		expectedEnd := runGitOutput("log", "-1", "--pretty=format:%aI", "HEAD")

		workStarted, _ := row["work_started"].(string)
		workEnded, _ := row["work_ended"].(string)
		if workStarted != expectedStart {
			t.Fatalf("expected work_started=%q, got %q", expectedStart, workStarted)
		}
		if workEnded != expectedEnd {
			t.Fatalf("expected work_ended=%q, got %q", expectedEnd, workEnded)
		}

		durationRaw, ok := row["duration_seconds"]
		if !ok {
			t.Fatalf("expected duration_seconds to be present for live commit")
		}
		duration, _ := durationRaw.(float64)
		if duration <= 0 {
			t.Fatalf("expected duration_seconds > 0, got %v", durationRaw)
		}
	})
}

func TestWatchOmitsWorkDurationForCatchupCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	withWorkspaceNoTickets(t, func(dir string) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		runGit := func(args ...string) {
			cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
			}
		}
		commitWithDate := func(name, message, date string) {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte(name+"\n"), 0644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
			runGit("add", name)
			cmd := exec.Command("git", "-C", dir, "commit", "-m", message)
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_DATE="+date,
				"GIT_COMMITTER_DATE="+date,
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git commit failed: %v\n%s", err, string(out))
			}
		}

		runGit("init")
		runGit("config", "user.email", "tkt@example.com")
		runGit("config", "user.name", "tkt")

		oldParent := time.Now().UTC().Add(-3 * time.Hour).Format(time.RFC3339)
		oldTagged := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
		commitWithDate("base.txt", "base", oldParent)
		commitWithDate("old.txt", "[dur-old-1] old commit", oldTagged)

		cfg := project.Config{
			Projects: map[string]project.ProjectConfig{
				"demo": {
					Path:         project.DetectProjectPath(dir),
					Store:        "local",
					AutoLink:     true,
					AutoClose:    true,
					RegisteredAt: time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339),
				},
			},
		}
		if err := project.Save(cfg); err != nil {
			t.Fatalf("save config: %v", err)
		}

		if _, _, err := runCmd(t, "", "watch", "--once"); err != nil {
			t.Fatalf("watch --once: %v", err)
		}

		journalPath := filepath.Join(home, ".tkt", "state", "demo", "commits.jsonl")
		entries := readJournalEntriesFromFile(t, journalPath)
		if len(entries) != 1 {
			t.Fatalf("expected 1 entry, got %d (%+v)", len(entries), entries)
		}

		row := entries[0]
		if got, _ := row["ticket"].(string); got != "dur-old-1" {
			t.Fatalf("expected ticket dur-old-1, got %q", got)
		}
		if ws, ok := row["work_started"]; ok && ws != "" {
			t.Fatalf("expected no work_started for catchup commit, got %v", ws)
		}
		if we, ok := row["work_ended"]; ok && we != "" {
			t.Fatalf("expected no work_ended for catchup commit, got %v", we)
		}
		if ds, ok := row["duration_seconds"]; ok {
			if n, _ := ds.(float64); n != 0 {
				t.Fatalf("expected no duration_seconds for catchup commit, got %v", ds)
			}
		}
	})
}

func TestWatchGlobalIteratesMultipleProjects(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	withWorkspaceNoTickets(t, func(_ string) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		setupRepo := func(name string) string {
			dir := filepath.Join(t.TempDir(), name)
			if err := os.MkdirAll(filepath.Join(dir, ".tickets"), 0755); err != nil {
				t.Fatalf("mkdir %s: %v", name, err)
			}
			runGit := func(args ...string) {
				cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v in %s failed: %v\n%s", args, name, err, string(out))
				}
			}
			runGit("init")
			runGit("config", "user.email", "tkt@example.com")
			runGit("config", "user.name", "tkt")
			return dir
		}

		commitInRepo := func(dir, filename, message string) {
			path := filepath.Join(dir, filename)
			if err := os.WriteFile(path, []byte(filename+"\n"), 0644); err != nil {
				t.Fatalf("write %s: %v", filename, err)
			}
			cmd := exec.Command("git", "-C", dir, "add", filename)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git add: %v\n%s", err, string(out))
			}
			cmd = exec.Command("git", "-C", dir, "commit", "-m", message)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git commit: %v\n%s", err, string(out))
			}
		}

		dirA := setupRepo("project-a")
		dirB := setupRepo("project-b")

		registration := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
		commitInRepo(dirA, "a.txt", "[ticket-a] commit in project A")
		commitInRepo(dirB, "b.txt", "[ticket-b] commit in project B")

		cfg := project.Config{
			Projects: map[string]project.ProjectConfig{
				"proj-a": {
					Path:         project.DetectProjectPath(dirA),
					Store:        "local",
					AutoLink:     true,
					AutoClose:    false,
					RegisteredAt: registration,
				},
				"proj-b": {
					Path:         project.DetectProjectPath(dirB),
					Store:        "local",
					AutoLink:     true,
					AutoClose:    false,
					RegisteredAt: registration,
				},
			},
		}
		if err := project.Save(cfg); err != nil {
			t.Fatalf("save config: %v", err)
		}

		// watch --once should process both projects
		if _, _, err := runCmd(t, "", "watch", "--once"); err != nil {
			t.Fatalf("watch --once: %v", err)
		}

		// Check journal for project A
		journalA := filepath.Join(home, ".tkt", "state", "proj-a", "commits.jsonl")
		entriesA := readJournalEntriesFromFile(t, journalA)
		if len(entriesA) != 1 {
			t.Fatalf("expected 1 entry in proj-a journal, got %d", len(entriesA))
		}
		if entriesA[0]["ticket"] != "ticket-a" {
			t.Fatalf("expected ticket-a, got %v", entriesA[0]["ticket"])
		}

		// Check journal for project B
		journalB := filepath.Join(home, ".tkt", "state", "proj-b", "commits.jsonl")
		entriesB := readJournalEntriesFromFile(t, journalB)
		if len(entriesB) != 1 {
			t.Fatalf("expected 1 entry in proj-b journal, got %d", len(entriesB))
		}
		if entriesB[0]["ticket"] != "ticket-b" {
			t.Fatalf("expected ticket-b, got %v", entriesB[0]["ticket"])
		}
	})
}

func TestWatchGlobalSkipsDisabledProjects(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	withWorkspaceNoTickets(t, func(_ string) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		dir := filepath.Join(t.TempDir(), "repo")
		if err := os.MkdirAll(filepath.Join(dir, ".tickets"), 0755); err != nil {
			t.Fatal(err)
		}
		runGit := func(args ...string) {
			cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
			}
		}
		runGit("init")
		runGit("config", "user.email", "tkt@example.com")
		runGit("config", "user.name", "tkt")

		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("f\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("add", "f.txt")
		runGit("commit", "-m", "[skip-1] should be ignored")

		cfg := project.Config{
			Projects: map[string]project.ProjectConfig{
				"disabled": {
					Path:         project.DetectProjectPath(dir),
					Store:        "local",
					AutoLink:     false,
					AutoClose:    false,
					RegisteredAt: time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339),
				},
			},
		}
		if err := project.Save(cfg); err != nil {
			t.Fatalf("save config: %v", err)
		}

		if _, _, err := runCmd(t, "", "watch", "--once"); err != nil {
			t.Fatalf("watch --once: %v", err)
		}

		// Journal should not exist — project was skipped
		journalPath := filepath.Join(home, ".tkt", "state", "disabled", "commits.jsonl")
		if _, err := os.Stat(journalPath); err == nil {
			t.Fatal("expected no journal for disabled project")
		}
	})
}

func TestWatchGlobalJournalsRefsAndAutoCloses(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	withWorkspaceNoTickets(t, func(_ string) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		setupRepo := func(name string) string {
			dir := filepath.Join(t.TempDir(), name)
			if err := os.MkdirAll(filepath.Join(dir, ".tickets"), 0755); err != nil {
				t.Fatalf("mkdir %s: %v", name, err)
			}
			runGit := func(args ...string) {
				cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v in %s failed: %v\n%s", args, name, err, string(out))
				}
			}
			runGit("init")
			runGit("config", "user.email", "tkt@example.com")
			runGit("config", "user.name", "tkt")
			return dir
		}

		commitInRepo := func(dir, filename, message string) {
			path := filepath.Join(dir, filename)
			if err := os.WriteFile(path, []byte(filename+"\n"), 0644); err != nil {
				t.Fatalf("write %s: %v", filename, err)
			}
			cmd := exec.Command("git", "-C", dir, "add", filename)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git add: %v\n%s", err, string(out))
			}
			cmd = exec.Command("git", "-C", dir, "commit", "-m", message)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git commit: %v\n%s", err, string(out))
			}
		}

		dirA := setupRepo("project-close")
		dirB := setupRepo("project-ref")

		// Seed a ticket in project A that should be auto-closed by the watcher.
		closeTicketPath := filepath.Join(dirA, ".tickets", "close-a.md")
		if err := os.WriteFile(closeTicketPath, []byte("---\nid: close-a\nstatus: open\ndeps: []\nlinks: []\ncreated: 2026-02-25T00:00:00Z\ntype: task\npriority: 1\n---\n# Close A\n"), 0644); err != nil {
			t.Fatalf("seed close-a ticket: %v", err)
		}

		registration := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
		commitInRepo(dirA, "a.txt", "Closes: [close-a] done")
		commitInRepo(dirB, "b.txt", "[ref-b] update")

		cfg := project.Config{
			Projects: map[string]project.ProjectConfig{
				"proj-close": {
					Path:         project.DetectProjectPath(dirA),
					Store:        "local",
					AutoLink:     true,
					AutoClose:    true,
					RegisteredAt: registration,
				},
				"proj-ref": {
					Path:         project.DetectProjectPath(dirB),
					Store:        "local",
					AutoLink:     true,
					AutoClose:    true,
					RegisteredAt: registration,
				},
			},
		}
		if err := project.Save(cfg); err != nil {
			t.Fatalf("save config: %v", err)
		}

		if _, _, err := runCmd(t, "", "watch", "--once"); err != nil {
			t.Fatalf("watch --once: %v", err)
		}

		closedRecord, err := ticket.LoadRecord(closeTicketPath)
		if err != nil {
			t.Fatalf("load close-a ticket: %v", err)
		}
		if closedRecord.Front.Status != "closed" {
			t.Fatalf("expected close-a ticket to be closed, got %s", closedRecord.Front.Status)
		}

		journalA := filepath.Join(home, ".tkt", "state", "proj-close", "commits.jsonl")
		entriesA := readJournalEntriesFromFile(t, journalA)
		if len(entriesA) != 1 {
			t.Fatalf("expected 1 entry in proj-close journal, got %d", len(entriesA))
		}
		if entriesA[0]["ticket"] != "close-a" {
			t.Fatalf("expected close-a entry, got %v", entriesA[0]["ticket"])
		}
		if entriesA[0]["action"] != "close" {
			t.Fatalf("expected close action, got %v", entriesA[0]["action"])
		}

		journalB := filepath.Join(home, ".tkt", "state", "proj-ref", "commits.jsonl")
		entriesB := readJournalEntriesFromFile(t, journalB)
		if len(entriesB) != 1 {
			t.Fatalf("expected 1 entry in proj-ref journal, got %d", len(entriesB))
		}
		if entriesB[0]["ticket"] != "ref-b" {
			t.Fatalf("expected ref-b entry, got %v", entriesB[0]["ticket"])
		}
		if entriesB[0]["action"] != "ref" {
			t.Fatalf("expected ref action, got %v", entriesB[0]["action"])
		}
	})
}

func TestSyncCentralStoreGitAbortsRebaseOnConflict(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	runGit := func(t *testing.T, repo string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git -C %s %v failed: %v\n%s", repo, args, err, string(out))
		}
	}

	// Set up a bare remote, a central store clone, and create a conflict.
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, ".", "init", "--bare", remote)

	store := filepath.Join(t.TempDir(), "store")
	runGit(t, ".", "clone", remote, store)
	runGit(t, store, "config", "user.email", "tkt@example.com")
	runGit(t, store, "config", "user.name", "tkt")

	// Seed the remote with an initial commit.
	ticketDir := filepath.Join(store, "demo")
	if err := os.MkdirAll(ticketDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ticketDir, "t1.md"), []byte("---\nid: t1\n---\n# T1 original\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, store, "add", "-A")
	runGit(t, store, "commit", "-m", "seed")
	runGit(t, store, "push")

	// Make a conflicting commit on the remote (via a second clone).
	store2 := filepath.Join(t.TempDir(), "store2")
	runGit(t, ".", "clone", remote, store2)
	runGit(t, store2, "config", "user.email", "tkt@example.com")
	runGit(t, store2, "config", "user.name", "tkt")
	if err := os.WriteFile(filepath.Join(store2, "demo", "t1.md"), []byte("---\nid: t1\n---\n# T1 remote change\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, store2, "add", "-A")
	runGit(t, store2, "commit", "-m", "remote edit")
	runGit(t, store2, "push")

	// Make a conflicting local change in the original store (uncommitted so
	// syncCentralStoreGit will add, commit, then hit the push/rebase conflict).
	if err := os.WriteFile(filepath.Join(ticketDir, "t1.md"), []byte("---\nid: t1\n---\n# T1 local change\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// syncCentralStoreGit should commit, fail to push, fail to rebase, and abort cleanly.
	result := syncCentralStoreGit(store)

	if !strings.Contains(result, "aborted rebase") {
		t.Fatalf("expected aborted rebase message, got: %q", result)
	}

	// Verify repo is NOT left in a mid-rebase state.
	rebaseDir := filepath.Join(store, ".git", "rebase-apply")
	rebaseMergeDir := filepath.Join(store, ".git", "rebase-merge")
	if _, err := os.Stat(rebaseDir); err == nil {
		t.Fatal("repo left in mid-rebase state (rebase-apply exists)")
	}
	if _, err := os.Stat(rebaseMergeDir); err == nil {
		t.Fatal("repo left in mid-rebase state (rebase-merge exists)")
	}
	statusOut, err := exec.Command("git", "-C", store, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git status after abort: %v", err)
	}
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Fatalf("expected clean repo after abort, got %q", string(statusOut))
	}
	if blocked := readCentralSyncBlocked(store); !strings.Contains(blocked, "central sync blocked:") {
		t.Fatalf("expected persisted blocked sync warning, got %q", blocked)
	}

	// Second cycle: the daemon should not retry the same conflict. It should
	// surface the persisted blocked state until the repo is resolved.
	result2 := syncCentralStoreGit(store)
	if !strings.Contains(result2, "central sync blocked:") {
		t.Fatalf("expected second cycle to stay blocked, got: %q", result2)
	}

	statusOut, err = exec.Command("git", "-C", store, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git status after blocked cycle: %v", err)
	}
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Fatalf("expected clean repo while blocked, got %q", string(statusOut))
	}
}

func TestSyncCentralStoreGitPushesWithoutConfiguredUpstream(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	runGit := func(t *testing.T, repo string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git -C %s %v failed: %v\n%s", repo, args, err, string(out))
		}
	}

	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, ".", "init", "--bare", remote)

	store := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(store, 0755); err != nil {
		t.Fatal(err)
	}
	runGit(t, store, "init")
	runGit(t, store, "config", "user.email", "tkt@example.com")
	runGit(t, store, "config", "user.name", "tkt")
	runGit(t, store, "remote", "add", "origin", remote)

	ticketDir := filepath.Join(store, "demo")
	if err := os.MkdirAll(ticketDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ticketDir, "t1.md"), []byte("---\nid: t1\n---\n# T1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	result := syncCentralStoreGit(store)
	if !strings.Contains(result, "central: committed") {
		t.Fatalf("expected sync commit info, got %q", result)
	}

	branchOut, err := exec.Command("git", "-C", store, "rev-parse", "--abbrev-ref", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("read current branch: %v\n%s", err, string(branchOut))
	}
	branch := strings.TrimSpace(string(branchOut))
	upstreamOut, err := exec.Command("git", "-C", store, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}").CombinedOutput()
	if err != nil {
		t.Fatalf("expected upstream to be configured: %v\n%s", err, string(upstreamOut))
	}
	if strings.TrimSpace(string(upstreamOut)) != "origin/"+branch {
		t.Fatalf("expected origin/%s upstream, got %q", branch, string(upstreamOut))
	}
}
