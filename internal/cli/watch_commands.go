package cli

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lawrips/tkt/internal/engine"
	"github.com/lawrips/tkt/internal/mcp"
	"github.com/lawrips/tkt/internal/project"
	"github.com/lawrips/tkt/internal/ticket"
)

type gitCommit struct {
	SHA    string
	TS     string
	Author string
	Msg    string
}

// runServe is the foreground serve command (equivalent to old runWatch foreground behavior).
func runServe(ctx context, args []string) error {
	return runWatchForeground(ctx, args, "serve")
}

// runWatch is kept as an alias for backward compatibility.
func runServeRun(ctx context, args []string) error {
	return runWatchForeground(ctx, args, "serve run")
}

func runWatchForeground(ctx context, args []string, cmdName string) error {
	fs := flag.NewFlagSet(cmdName, flag.ContinueOnError)
	fs.SetOutput(ctx.stderr)

	once := false
	interval := 5 * time.Second
	noMCP := false
	noWatcher := false
	fs.BoolVar(&once, "once", false, "")
	fs.DurationVar(&interval, "interval", 5*time.Second, "")
	fs.BoolVar(&noMCP, "no-mcp", false, "")
	fs.BoolVar(&noWatcher, "no-watcher", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: tkt %s [--once] [--interval=5s] [--no-mcp] [--no-watcher]", cmdName)
	}
	if interval <= 0 {
		return fmt.Errorf("--interval must be > 0")
	}

	if !ctx.json {
		cfg, _ := project.Load()
		_, _ = fmt.Fprintf(
			ctx.stderr,
			"%s starting: projects=%d interval=%s watcher=%t mcp=%t once=%t\n",
			cmdName,
			len(cfg.Projects),
			interval,
			!noWatcher,
			!noMCP,
			once,
		)
		defer func() {
			_, _ = fmt.Fprintf(ctx.stderr, "%s stopping\n", cmdName)
		}()
	}

	cycle := func() error {
		if noWatcher {
			return nil
		}
		cfg, err := project.Load()
		if err != nil {
			_, _ = fmt.Fprintf(ctx.stderr, "warning: failed to load project config: %v\n", err)
			return nil
		}
		totalAppended := 0
		totalClosed := 0
		allWarnings := make([]string, 0)
		for name, entry := range cfg.Projects {
			if !entry.AutoLink && !entry.AutoClose && entry.Store != "central" {
				continue
			}
			appended, closed, warnings, err := runWatchCycle(name, entry)
			if err != nil {
				allWarnings = append(allWarnings, fmt.Sprintf("%s: %v", name, err))
				continue
			}
			totalAppended += appended
			totalClosed += closed
			for _, w := range warnings {
				allWarnings = append(allWarnings, fmt.Sprintf("%s: %s", name, w))
			}
		}
		if ctx.json {
			return emitJSON(ctx, map[string]any{
				"entries_added":  totalAppended,
				"tickets_closed": totalClosed,
				"warnings":       allWarnings,
				"watch_interval": interval.String(),
			})
		}
		if totalAppended > 0 || totalClosed > 0 || len(allWarnings) > 0 {
			_, _ = fmt.Fprintf(ctx.stderr, "watch cycle complete: appended %d entries, auto-closed %d ticket(s)\n", totalAppended, totalClosed)
			for _, msg := range allWarnings {
				_, _ = fmt.Fprintf(ctx.stderr, "  %s\n", msg)
			}
		}
		return nil
	}

	if once {
		return cycle()
	}

	pidPath, err := servePIDPath()
	if err == nil {
		_ = os.MkdirAll(filepath.Dir(pidPath), 0755)
		_ = os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0644)
		defer os.Remove(pidPath)
	}

	if !noMCP {
		// MCP needs a project context — resolve from cwd.
		_, mcpProject, mcpEntry, resolveErr := resolveCurrentProject(ctx)
		if resolveErr == nil {
			ticketDir, dirErr := ticketStoreDir(mcpProject, mcpEntry)
			if dirErr == nil {
				mcpSrv := mcp.NewServer(mcpProject, ticketDir)
				go func() {
					if serveErr := mcpSrv.ServeStdio(); serveErr != nil {
						_, _ = fmt.Fprintf(ctx.stderr, "mcp: %v\n", serveErr)
					}
				}()
			}
		}
	}

	for {
		if err := cycle(); err != nil {
			return err
		}
		time.Sleep(interval)
	}
}

func runServeStart(ctx context, args []string) error {
	fs := flag.NewFlagSet("serve start", flag.ContinueOnError)
	fs.SetOutput(ctx.stderr)
	interval := 5 * time.Second
	noWatcher := false
	fs.DurationVar(&interval, "interval", 5*time.Second, "")
	fs.BoolVar(&noWatcher, "no-watcher", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: tkt serve start [--interval=5s] [--no-watcher]")
	}

	pidPath, err := servePIDPath()
	if err != nil {
		return err
	}
	logPath, err := serveLogPath()
	if err != nil {
		return err
	}

	if pid, running := serveRunningPID(pidPath); running {
		if ctx.json {
			return emitJSON(ctx, map[string]any{
				"status": "already_running",
				"pid":    pid,
			})
		}
		_, _ = fmt.Fprintf(ctx.stdout, "serve already running (pid %d)\n", pid)
		return nil
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find executable: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}

	childArgs := []string{self, "serve", "run", "--interval=" + interval.String(), "--no-mcp"}
	if noWatcher {
		childArgs = append(childArgs, "--no-watcher")
	}

	procAttr := &os.ProcAttr{
		Files: []*os.File{
			nil,
			logFile,
			logFile,
		},
	}

	proc, err := os.StartProcess(self, childArgs, procAttr)
	logFile.Close()
	if err != nil {
		return fmt.Errorf("start serve process: %w", err)
	}
	_ = proc.Release()

	if ctx.json {
		return emitJSON(ctx, map[string]any{
			"status":   "started",
			"pid_file": pidPath,
			"log_file": logPath,
		})
	}

	_, _ = fmt.Fprintf(ctx.stdout, "serve started (log: %s)\n", logPath)
	return nil
}

func runServeStop(ctx context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: tkt serve stop")
	}

	pidPath, err := servePIDPath()
	if err != nil {
		return err
	}

	pid, running := serveRunningPID(pidPath)
	if !running {
		if ctx.json {
			return emitJSON(ctx, map[string]any{"status": "not_running"})
		}
		_, _ = fmt.Fprintln(ctx.stdout, "serve is not running")
		return nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	if err := proc.Signal(os.Interrupt); err != nil {
		return fmt.Errorf("signal process %d: %w", pid, err)
	}
	_ = os.Remove(pidPath)

	if ctx.json {
		return emitJSON(ctx, map[string]any{"status": "stopped", "pid": pid})
	}

	_, _ = fmt.Fprintf(ctx.stdout, "serve stopped (pid %d)\n", pid)
	return nil
}

func runServeStatus(ctx context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: tkt serve status")
	}

	pidPath, err := servePIDPath()
	if err != nil {
		return err
	}
	logPath, err := serveLogPath()
	if err != nil {
		return err
	}

	pid, running := serveRunningPID(pidPath)

	if ctx.json {
		return emitJSON(ctx, map[string]any{
			"running":  running,
			"pid":      pid,
			"pid_file": pidPath,
			"log_file": logPath,
		})
	}

	if running {
		_, _ = fmt.Fprintf(ctx.stdout, "serve running (pid %d)\n", pid)
	} else {
		_, _ = fmt.Fprintln(ctx.stdout, "serve is not running")
	}
	_, _ = fmt.Fprintf(ctx.stdout, "log: %s\n", logPath)
	return nil
}

func runServeLogs(ctx context, args []string) error {
	fs := flag.NewFlagSet("serve logs", flag.ContinueOnError)
	fs.SetOutput(ctx.stderr)
	lines := 50
	fs.IntVar(&lines, "n", 50, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: tkt serve logs [-n=50]")
	}

	logPath, err := serveLogPath()
	if err != nil {
		return err
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			if ctx.json {
				return emitJSON(ctx, map[string]any{
					"log_file": logPath,
					"lines":    []string{},
				})
			}
			_, _ = fmt.Fprintln(ctx.stdout, "(no log file)")
			return nil
		}
		return err
	}

	allLines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	start := 0
	if len(allLines) > lines {
		start = len(allLines) - lines
	}
	tail := allLines[start:]

	if ctx.json {
		return emitJSON(ctx, map[string]any{
			"log_file": logPath,
			"lines":    tail,
		})
	}

	for _, line := range tail {
		_, _ = fmt.Fprintln(ctx.stdout, line)
	}
	return nil
}

func servePIDPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".tkt", "state", "serve.pid"), nil
}

func serveLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".tkt", "state", "serve.log"), nil
}

func serveRunningPID(pidPath string) (int, bool) {
	raw, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return 0, false
	}
	return pid, true
}

func runWatchCycle(projectName string, entry project.ProjectConfig) (int, int, []string, error) {
	jPath, err := engine.JournalPath(projectName)
	if err != nil {
		return 0, 0, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(jPath), 0755); err != nil {
		return 0, 0, nil, err
	}

	knownSHAs, lastSHA, err := loadJournalState(jPath)
	if err != nil {
		return 0, 0, nil, err
	}

	if !dirExists(filepath.Join(entry.Path, ".git")) {
		return 0, 0, nil, fmt.Errorf("no git repository at %s — auto-link and auto-close require a git repo", entry.Path)
	}

	commits, err := collectCommitsForWatch(entry.Path, lastSHA, entry.RegisteredAt)
	if err != nil {
		return 0, 0, nil, err
	}

	ticketDir, err := ticketStoreDir(projectName, entry)
	if err != nil {
		return 0, 0, nil, err
	}

	toAppend := make([]engine.CommitJournalEntry, 0)
	closedCount := 0
	warnings := make([]string, 0)

	type diffResult struct {
		files        []string
		added        int
		removed      int
		branch       string
		workStarted  string
		durationSecs int
	}
	diffCache := make(map[string]diffResult)

	for _, commit := range commits {
		if _, seen := knownSHAs[commit.SHA]; seen {
			continue
		}

		actions := extractTicketActions(commit.Msg)
		if len(actions) == 0 {
			continue
		}

		// Fetch diff stats once per SHA, shared across all ticket entries for that commit.
		if _, cached := diffCache[commit.SHA]; !cached {
			files, added, removed, branch, derr := getDiffStats(entry.Path, commit.SHA)
			if derr != nil {
				warnings = append(warnings, fmt.Sprintf("diff-tree %s failed: %v", engine.ShortSHA(commit.SHA), derr))
			}
			workStarted := ""
			durationSecs := 0
			if isLiveCommit(commit.TS) {
				workStarted = getParentCommitTS(entry.Path, commit.SHA)
				durationSecs = workDurationSeconds(workStarted, commit.TS)
			}
			diffCache[commit.SHA] = diffResult{
				files:        files,
				added:        added,
				removed:      removed,
				branch:       branch,
				workStarted:  workStarted,
				durationSecs: durationSecs,
			}
		}
		dr := diffCache[commit.SHA]

		tickets := make([]string, 0, len(actions))
		for ticketID := range actions {
			tickets = append(tickets, ticketID)
		}
		sort.Strings(tickets)

		for _, ticketID := range tickets {
			action := actions[ticketID]
			if action == "close" && entry.AutoClose {
				changed, err := autoCloseTicket(ticketDir, ticketID)
				if err != nil {
					warnings = append(warnings, fmt.Sprintf("auto-close %s failed: %v", ticketID, err))
				} else if changed {
					closedCount++
				}
			}

			if !entry.AutoLink {
				continue
			}

			workEnded := ""
			if dr.workStarted != "" {
				workEnded = commit.TS
			}

			toAppend = append(toAppend, engine.CommitJournalEntry{
				SHA:          commit.SHA,
				Ticket:       ticketID,
				Repo:         entry.Path,
				TS:           commit.TS,
				Msg:          commit.Msg,
				Author:       commit.Author,
				Action:       action,
				FilesChanged: dr.files,
				LinesAdded:   dr.added,
				LinesRemoved: dr.removed,
				Branch:       dr.branch,
				WorkStarted:  dr.workStarted,
				WorkEnded:    workEnded,
				DurationSecs: dr.durationSecs,
			})
		}

		if entry.AutoLink {
			knownSHAs[commit.SHA] = struct{}{}
		}
	}

	if len(toAppend) > 0 {
		f, err := os.OpenFile(jPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return 0, 0, nil, err
		}
		defer f.Close()

		enc := json.NewEncoder(f)
		for _, row := range toAppend {
			if err := enc.Encode(row); err != nil {
				return 0, 0, nil, err
			}
		}
	}

	if entry.Store == "central" {
		centralRoot, err := centralStoreRootDir()
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("resolve central store root failed: %v", err))
		} else if msg := syncCentralStoreGit(centralRoot); msg != "" {
			warnings = append(warnings, msg)
		}
	}

	return len(toAppend), closedCount, warnings, nil
}

func loadJournalState(path string) (map[string]struct{}, string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]struct{}{}, "", nil
		}
		return nil, "", err
	}
	defer f.Close()

	known := map[string]struct{}{}
	lastSHA := ""

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row engine.CommitJournalEntry
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		if row.SHA == "" {
			continue
		}
		known[row.SHA] = struct{}{}
		lastSHA = row.SHA
	}
	if err := scanner.Err(); err != nil {
		return nil, "", err
	}

	return known, lastSHA, nil
}

func collectCommitsForWatch(repoPath string, lastSHA string, registeredAt string) ([]gitCommit, error) {
	args := []string{"-C", repoPath, "log", "--reverse", "--pretty=format:%H%x1f%cI%x1f%an%x1f%B%x1e"}
	if strings.TrimSpace(lastSHA) != "" {
		args = append(args, fmt.Sprintf("%s..HEAD", strings.TrimSpace(lastSHA)))
	} else if strings.TrimSpace(registeredAt) != "" {
		args = append(args, "--since="+strings.TrimSpace(registeredAt))
	}

	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		if strings.TrimSpace(lastSHA) != "" {
			// History may have been rewritten. Fall back to registration timestamp.
			return collectCommitsForWatch(repoPath, "", registeredAt)
		}
		return nil, fmt.Errorf("git log failed at %s: %v", repoPath, err)
	}

	records := strings.Split(string(out), "\x1e")
	outCommits := make([]gitCommit, 0, len(records))
	for _, record := range records {
		record = strings.TrimSpace(record)
		if record == "" {
			continue
		}
		parts := strings.SplitN(record, "\x1f", 4)
		if len(parts) < 4 {
			continue
		}
		outCommits = append(outCommits, gitCommit{
			SHA:    parts[0],
			TS:     parts[1],
			Author: parts[2],
			Msg:    strings.TrimSpace(parts[3]),
		})
	}
	return outCommits, nil
}

func extractTicketActions(message string) map[string]string {
	refPattern := regexp.MustCompile(`\[([A-Za-z0-9][A-Za-z0-9_-]*)\]`)
	closePattern := regexp.MustCompile(`(?i)\b(?:closes|fixes)\s*:?\s*\[([A-Za-z0-9][A-Za-z0-9_-]*)\]`)

	actions := map[string]string{}
	for _, match := range closePattern.FindAllStringSubmatch(message, -1) {
		if len(match) > 1 {
			actions[match[1]] = "close"
		}
	}
	for _, match := range refPattern.FindAllStringSubmatch(message, -1) {
		if len(match) > 1 {
			if _, ok := actions[match[1]]; !ok {
				actions[match[1]] = "ref"
			}
		}
	}
	return actions
}

func ticketStoreDir(projectName string, entry project.ProjectConfig) (string, error) {
	if entry.Store == "central" {
		return centralProjectDir(projectName)
	}
	return filepath.Join(entry.Path, ".tickets"), nil
}

func autoCloseTicket(dir string, ticketID string) (bool, error) {
	path := filepath.Join(dir, ticketID+".md")
	record, err := ticket.LoadRecord(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if record.Front.Status == "closed" {
		return false, nil
	}
	record.Front.Status = "closed"
	if err := ticket.SaveRecord(record); err != nil {
		return false, err
	}
	return true, nil
}

func syncCentralStoreGit(storeRoot string) string {
	if err := os.MkdirAll(storeRoot, 0755); err != nil {
		return fmt.Sprintf("ensure central store directory failed: %v", err)
	}

	if !dirExists(filepath.Join(storeRoot, ".git")) {
		if out, err := exec.Command("git", "-C", storeRoot, "init").CombinedOutput(); err != nil {
			return fmt.Sprintf("git init failed: %v (%s)", err, strings.TrimSpace(string(out)))
		}
	}
	if warning := ensureGitIdentity(storeRoot); warning != "" {
		return warning
	}

	remoteNames, err := centralStoreRemoteNames(storeRoot)
	if err != nil {
		return fmt.Sprintf("git remote failed: %v", err)
	}
	remoteConfigured := len(remoteNames) > 0
	if remoteConfigured {
		if blocked := readCentralSyncBlocked(storeRoot); blocked != "" {
			if centralSyncBlockResolved(storeRoot) {
				_ = clearCentralSyncBlocked(storeRoot)
			} else {
				return blocked
			}
		}
	}

	if out, err := exec.Command("git", "-C", storeRoot, "add", "-A").CombinedOutput(); err != nil {
		return fmt.Sprintf("git add failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	var info string
	diff := exec.Command("git", "-C", storeRoot, "diff", "--cached", "--quiet")
	if err := diff.Run(); err != nil {
		// Staged changes — commit them.
		commitMsg := buildCentralCommitMessage(storeRoot)
		if out, err := exec.Command("git", "-C", storeRoot, "commit", "-m", commitMsg).CombinedOutput(); err != nil {
			return fmt.Sprintf("git commit failed: %v (%s)", err, strings.TrimSpace(string(out)))
		}
		info = fmt.Sprintf("central: committed (%s)", commitMsg)
	}

	if !remoteConfigured {
		return info
	}

	// Check if there are unpushed commits (covers both fresh commits and
	// previous cycles where push/rebase failed and left commits behind).
	shouldPush := info != ""
	if !shouldPush {
		out, err := exec.Command("git", "-C", storeRoot, "rev-list", "--count", "@{u}..HEAD").CombinedOutput()
		if err == nil {
			shouldPush = strings.TrimSpace(string(out)) != "0"
		} else if isNoUpstreamError(string(out)) {
			shouldPush = true
		} else {
			return fmt.Sprintf("git upstream status check failed (%s)", strings.TrimSpace(string(out)))
		}
	}
	if !shouldPush {
		_ = clearCentralSyncBlocked(storeRoot)
		return info
	}

	if out, err := pushCentralStoreGit(storeRoot, remoteNames[0]); err == nil {
		_ = clearCentralSyncBlocked(storeRoot)
		return info
	} else {
		_, _ = out, err
	}

	// Push failed — attempt pull --rebase then retry.
	if out, err := exec.Command("git", "-C", storeRoot, "pull", "--rebase").CombinedOutput(); err != nil {
		// Rebase failed — abort to leave repo in a clean state.
		_, _ = exec.Command("git", "-C", storeRoot, "rebase", "--abort").CombinedOutput()
		msg := fmt.Sprintf("central sync blocked: git pull --rebase failed; aborted rebase to keep repo clean (%s)", strings.TrimSpace(string(out)))
		_ = writeCentralSyncBlocked(storeRoot, msg)
		return msg
	}

	if out, err := pushCentralStoreGit(storeRoot, remoteNames[0]); err != nil {
		return fmt.Sprintf("git push failed after rebase (%s)", strings.TrimSpace(string(out)))
	}
	_ = clearCentralSyncBlocked(storeRoot)

	return info
}

func centralStoreRemoteNames(storeRoot string) ([]string, error) {
	out, err := exec.Command("git", "-C", storeRoot, "remote").Output()
	if err != nil {
		return nil, err
	}
	names := strings.Fields(strings.TrimSpace(string(out)))
	return names, nil
}

func pushCentralStoreGit(storeRoot, remoteName string) ([]byte, error) {
	out, err := exec.Command("git", "-C", storeRoot, "push").CombinedOutput()
	if err == nil || !isNoUpstreamError(string(out)) {
		return out, err
	}

	branchOut, branchErr := exec.Command("git", "-C", storeRoot, "rev-parse", "--abbrev-ref", "HEAD").CombinedOutput()
	if branchErr != nil {
		return out, err
	}
	branch := strings.TrimSpace(string(branchOut))
	if branch == "" || branch == "HEAD" {
		return out, err
	}

	return exec.Command("git", "-C", storeRoot, "push", "-u", remoteName, branch).CombinedOutput()
}

func isNoUpstreamError(out string) bool {
	return strings.Contains(out, "has no upstream branch") || strings.Contains(out, "no upstream branch")
}

func centralSyncBlockedPath(storeRoot string) string {
	return filepath.Join(storeRoot, ".git", "tkt-central-sync-blocked")
}

func readCentralSyncBlocked(storeRoot string) string {
	raw, err := os.ReadFile(centralSyncBlockedPath(storeRoot))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func writeCentralSyncBlocked(storeRoot, msg string) error {
	return os.WriteFile(centralSyncBlockedPath(storeRoot), []byte(strings.TrimSpace(msg)+"\n"), 0644)
}

func clearCentralSyncBlocked(storeRoot string) error {
	if err := os.Remove(centralSyncBlockedPath(storeRoot)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func centralSyncBlockResolved(storeRoot string) bool {
	if dirExists(filepath.Join(storeRoot, ".git", "rebase-apply")) || dirExists(filepath.Join(storeRoot, ".git", "rebase-merge")) {
		return false
	}

	statusOut, err := exec.Command("git", "-C", storeRoot, "status", "--porcelain").Output()
	if err != nil || strings.TrimSpace(string(statusOut)) != "" {
		return false
	}

	aheadOut, err := exec.Command("git", "-C", storeRoot, "rev-list", "--count", "@{u}..HEAD").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(aheadOut)) == "0"
}

func buildCentralCommitMessage(storeRoot string) string {
	out, err := exec.Command("git", "-C", storeRoot, "diff", "--cached", "--name-only").Output()
	if err != nil {
		return "tkt: sync ticket changes"
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	ids := make([]string, 0, len(lines))
	for _, line := range lines {
		name := filepath.Base(line)
		if strings.HasSuffix(name, ".md") {
			ids = append(ids, strings.TrimSuffix(name, ".md"))
		}
	}
	if len(ids) == 0 {
		return "tkt: sync ticket changes"
	}
	if len(ids) <= 3 {
		return fmt.Sprintf("tkt: sync %s", strings.Join(ids, ", "))
	}
	return fmt.Sprintf("tkt: sync %s (+%d more)", strings.Join(ids[:3], ", "), len(ids)-3)
}

func ensureGitIdentity(repoPath string) string {
	email := strings.TrimSpace(commandOutput("git", "-C", repoPath, "config", "--get", "user.email"))
	if email == "" {
		if out, err := exec.Command("git", "-C", repoPath, "config", "user.email", "tkt@local").CombinedOutput(); err != nil {
			return fmt.Sprintf("set git user.email failed: %v (%s)", err, strings.TrimSpace(string(out)))
		}
	}

	name := strings.TrimSpace(commandOutput("git", "-C", repoPath, "config", "--get", "user.name"))
	if name == "" {
		if out, err := exec.Command("git", "-C", repoPath, "config", "user.name", "tkt").CombinedOutput(); err != nil {
			return fmt.Sprintf("set git user.name failed: %v (%s)", err, strings.TrimSpace(string(out)))
		}
	}
	return ""
}

func commandOutput(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// isLiveCommit returns true if the commit timestamp is within the last 60 seconds,
// meaning the watcher is running live rather than catching up on old commits.
// File mtimes on disk are only meaningful in live mode.
func isLiveCommit(ts string) bool {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return false
	}
	return time.Since(t) < 60*time.Second
}

func workDurationSeconds(startTS, endTS string) int {
	if startTS == "" || endTS == "" {
		return 0
	}
	start, err := time.Parse(time.RFC3339, startTS)
	if err != nil {
		return 0
	}
	end, err := time.Parse(time.RFC3339, endTS)
	if err != nil {
		return 0
	}
	if !end.After(start) {
		return 0
	}
	return int(end.Sub(start).Seconds())
}
