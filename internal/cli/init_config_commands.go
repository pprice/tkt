package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lawrips/tkt/internal/project"
)

func runInit(ctx context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(ctx.stderr)

	var projectFlag string
	var storeFlag string
	autoLink := true
	autoClose := true
	yes := false

	fs.StringVar(&projectFlag, "project", "", "")
	fs.StringVar(&storeFlag, "store", "", "")
	fs.BoolVar(&autoLink, "auto-link", true, "")
	fs.BoolVar(&autoClose, "auto-close", true, "")
	fs.BoolVar(&yes, "yes", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: tkt init [--project <name>] [--store central|local] [--auto-link=true|false] [--auto-close=true|false] [--yes]")
	}
	if storeFlag != "" && storeFlag != "central" && storeFlag != "local" {
		return fmt.Errorf("--store must be central or local")
	}

	providedProject := ctx.projectOverride
	if strings.TrimSpace(projectFlag) != "" {
		providedProject = strings.TrimSpace(projectFlag)
	}

	cfg, err := project.Load()
	if err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	repoPath := project.DetectProjectPath(cwd)
	localTicketsDir := filepath.Join(repoPath, ".tickets")
	hasLocalTickets := dirExists(localTicketsDir)

	// Git health check
	hasGit := dirExists(filepath.Join(repoPath, ".git"))
	if !hasGit {
		_, _ = fmt.Fprintln(ctx.stderr, "! No git repository detected. Auto-link and auto-close require git.")
		if !flagProvided(args, "--auto-link") {
			autoLink = false
		}
		if !flagProvided(args, "--auto-close") {
			autoClose = false
		}
	}

	store := storeFlag
	copyLocalToCentral := false

	interactive := !yes
	if store == "" {
		if hasLocalTickets {
			if interactive {
				_, _ = fmt.Fprintln(ctx.stdout, "Existing tickets found in .tickets/")
				_, _ = fmt.Fprintln(ctx.stdout)
				centralRootDisplay, err := centralStoreRootDir()
				if err != nil {
					return fmt.Errorf("cannot resolve central store path: %w", err)
				}
				_, _ = fmt.Fprintf(ctx.stdout, "  [1] Copy to central store at %s\n", centralRootDisplay)
				_, _ = fmt.Fprintln(ctx.stdout, "      Copies .md files; originals kept as backup")
				_, _ = fmt.Fprintln(ctx.stdout, "  [2] Keep local in .tickets/ inside this repo")
				_, _ = fmt.Fprintln(ctx.stdout)
				choice, err := promptChoice(ctx, "Choose [1/2]:", "1")
				if err != nil {
					return err
				}
				if choice == "2" {
					store = "local"
				} else {
					store = "central"
					copyLocalToCentral = true
				}
			} else {
				store = "central"
				copyLocalToCentral = true
			}
		} else {
			if interactive {
				_, _ = fmt.Fprintln(ctx.stdout, "Where should tickets be stored?")
				_, _ = fmt.Fprintln(ctx.stdout)
				centralRootDisplay, err := centralStoreRootDir()
				if err != nil {
					return fmt.Errorf("cannot resolve central store path: %w", err)
				}
				_, _ = fmt.Fprintf(ctx.stdout, "  [1] Central store at %s\n", centralRootDisplay)
				_, _ = fmt.Fprintln(ctx.stdout, "      Separate git repo, keeps project history clean")
				_, _ = fmt.Fprintln(ctx.stdout, "  [2] Local .tickets/ inside this repo")
				_, _ = fmt.Fprintln(ctx.stdout)
				choice, err := promptChoice(ctx, "Choose [1/2]:", "1")
				if err != nil {
					return err
				}
				if choice == "2" {
					store = "local"
				} else {
					store = "central"
				}
			} else {
				store = "central"
			}
		}
	} else if store == "central" && hasLocalTickets {
		copyLocalToCentral = true
	}

	providedAutoLink := flagProvided(args, "--auto-link")
	providedAutoClose := flagProvided(args, "--auto-close")

	if interactive && hasGit && !providedAutoLink {
		_, _ = fmt.Fprintln(ctx.stdout)
		_, _ = fmt.Fprintln(ctx.stdout, "Watches git log for [ticket-id] in commit messages and links them to tickets.")
		answer, err := promptYesNo(ctx, "Auto-link commits to tickets? (Default: yes)", true)
		if err != nil {
			return err
		}
		autoLink = answer
	}
	if interactive && hasGit && !providedAutoClose {
		_, _ = fmt.Fprintln(ctx.stdout)
		_, _ = fmt.Fprintln(ctx.stdout, "Closes tickets automatically when \"Closes: [ticket-id]\" appears in a commit message.")
		answer, err := promptYesNo(ctx, "Auto-close tickets? (Default: yes)", true)
		if err != nil {
			return err
		}
		autoClose = answer
	}

	projectName, _ := project.ResolveName(cfg, repoPath, providedProject)
	if projectName == "" {
		return errors.New("failed to resolve project name")
	}
	registeredAt := time.Now().UTC().Format(time.RFC3339)
	if existing, ok := cfg.Projects[projectName]; ok && strings.TrimSpace(existing.RegisteredAt) != "" {
		registeredAt = existing.RegisteredAt
	}

	var centralDir string
	var gitIdentitySet bool
	if store == "central" {
		centralRoot, err := centralStoreRootDir()
		if err != nil {
			return err
		}
		centralDir = filepath.Join(centralRoot, projectName)
		if err := os.MkdirAll(centralDir, 0755); err != nil {
			return err
		}
		if copyLocalToCentral && dirExists(localTicketsDir) {
			if err := copyTicketFiles(localTicketsDir, centralDir); err != nil {
				return err
			}
		}
		result, err := bootstrapCentralStoreGit(centralRoot)
		if err != nil {
			return err
		}
		gitIdentitySet = result.identitySet
	}
	if store == "local" && !dirExists(localTicketsDir) {
		if err := os.MkdirAll(localTicketsDir, 0755); err != nil {
			return err
		}
	}

	cfg.UpsertProject(projectName, project.ProjectConfig{
		Path:         repoPath,
		Store:        store,
		AutoLink:     autoLink,
		AutoClose:    autoClose,
		RegisteredAt: registeredAt,
	})
	if err := project.Save(cfg); err != nil {
		return err
	}
	if err := project.EnsureWorkflowFile(); err != nil {
		return err
	}
	workflowPath, err := project.WorkflowPath()
	if err != nil {
		return err
	}
	workflowDisplayPath, err := project.WorkflowDisplayPath()
	if err != nil {
		return err
	}

	// Resolve binary path for MCP commands
	self, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}

	if ctx.json {
		return emitJSON(ctx, map[string]any{
			"project":                 projectName,
			"path":                    repoPath,
			"store":                   store,
			"config":                  cfg.Projects[projectName],
			"workflow_path":           workflowPath,
			"has_git":                 hasGit,
			"copied_local_to_central": copyLocalToCentral,
			"executable":              self,
		})
	}

	// --- Status report ---
	_, _ = fmt.Fprintln(ctx.stdout)
	_, _ = fmt.Fprintln(ctx.stdout, "── Setup complete ──")
	_, _ = fmt.Fprintln(ctx.stdout)
	if store == "central" {
		_, _ = fmt.Fprintf(ctx.stdout, "  ✓ Central store initialized at %s\n", centralDir)
	} else {
		_, _ = fmt.Fprintf(ctx.stdout, "  ✓ Local store initialized at %s\n", localTicketsDir)
	}
	if copyLocalToCentral {
		_, _ = fmt.Fprintln(ctx.stdout, "  ✓ Tickets copied to central store")
		_, _ = fmt.Fprintln(ctx.stdout, "    Original .tickets/ kept as backup — remove when ready.")
	}
	if gitIdentitySet {
		gitIdentityRoot, err := centralStoreRootDir()
		if err == nil {
			_, _ = fmt.Fprintf(ctx.stdout, "  ✓ Git identity set to tkt@local in %s (no existing identity found)\n", gitIdentityRoot)
		}
	}
	_, _ = fmt.Fprintln(ctx.stdout, "  ✓ Project registered in ~/.tkt/config.yaml")
	if !hasGit {
		_, _ = fmt.Fprintln(ctx.stdout, "  ! No git repo — auto_link/auto_close disabled")
	}
	_, _ = fmt.Fprintf(ctx.stdout, "  ✓ Workflow guide available at %s\n", workflowDisplayPath)
	_, _ = fmt.Fprintln(ctx.stdout)

	return nil
}

type bootstrapResult struct {
	identitySet bool // true if git identity was configured by tkt (not already present)
}

func bootstrapCentralStoreGit(storeRoot string) (bootstrapResult, error) {
	var result bootstrapResult
	if err := os.MkdirAll(storeRoot, 0755); err != nil {
		return result, err
	}
	if !dirExists(filepath.Join(storeRoot, ".git")) {
		if out, err := exec.Command("git", "-C", storeRoot, "init").CombinedOutput(); err != nil {
			return result, fmt.Errorf("central store git init failed: %v (%s)", err, strings.TrimSpace(string(out)))
		}
	}

	// Check if identity already exists before ensuring it
	email := strings.TrimSpace(commandOutput("git", "-C", storeRoot, "config", "--get", "user.email"))
	if email == "" {
		result.identitySet = true
	}

	if warning := ensureGitIdentity(storeRoot); warning != "" {
		return result, fmt.Errorf("central store git identity setup failed: %s", warning)
	}

	if out, err := exec.Command("git", "-C", storeRoot, "add", "-A").CombinedOutput(); err != nil {
		return result, fmt.Errorf("central store git add failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	diff := exec.Command("git", "-C", storeRoot, "diff", "--cached", "--quiet")
	if err := diff.Run(); err == nil {
		return result, nil
	}

	if out, err := exec.Command("git", "-C", storeRoot, "commit", "-m", "tkt: init central store").CombinedOutput(); err != nil {
		return result, fmt.Errorf("central store git commit failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return result, nil
}

func runConfig(ctx context, args []string) error {
	if len(args) > 0 && args[0] == "set" {
		return runConfigSet(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "resolve" {
		return runConfigResolve(ctx, args[1:])
	}

	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	fs.SetOutput(ctx.stderr)
	projectFlag := ""
	showAll := false
	fs.StringVar(&projectFlag, "project", "", "")
	fs.BoolVar(&showAll, "all", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: tkt config [--project <name>] [--all]")
	}

	cfg, err := project.Load()
	if err != nil {
		return err
	}

	cwd, _ := os.Getwd()
	resolved, source := project.ResolveName(cfg, cwd, firstNonEmpty(strings.TrimSpace(projectFlag), strings.TrimSpace(ctx.projectOverride)))

	if ctx.json {
		if showAll {
			return emitJSON(ctx, map[string]any{
				"projects":          cfg.Projects,
				"resolved_project":  resolved,
				"resolution_source": source,
			})
		}

		selected := resolved
		if strings.TrimSpace(projectFlag) != "" {
			selected = strings.TrimSpace(projectFlag)
		}
		if selected == "" {
			return emitJSON(ctx, map[string]any{
				"projects":          map[string]project.ProjectConfig{},
				"resolved_project":  resolved,
				"resolution_source": source,
			})
		}
		item, ok := cfg.Projects[selected]
		if !ok {
			return fmt.Errorf("project %q not found in config", selected)
		}
		return emitJSON(ctx, map[string]any{
			"projects": map[string]project.ProjectConfig{
				selected: item,
			},
			"resolved_project":  resolved,
			"resolution_source": source,
		})
	}

	if len(cfg.Projects) == 0 {
		_, _ = fmt.Fprintln(ctx.stdout, "No projects configured.")
		return nil
	}

	if showAll {
		keys := make([]string, 0, len(cfg.Projects))
		for key := range cfg.Projects {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			p := cfg.Projects[key]
			_, _ = fmt.Fprintf(ctx.stdout, "%s\n", key)
			_, _ = fmt.Fprintf(ctx.stdout, "  path: %s\n", p.Path)
			_, _ = fmt.Fprintf(ctx.stdout, "  store: %s\n", p.Store)
			_, _ = fmt.Fprintf(ctx.stdout, "  registered_at: %s\n", p.RegisteredAt)
			_, _ = fmt.Fprintf(ctx.stdout, "  auto_link: %t\n", p.AutoLink)
			_, _ = fmt.Fprintf(ctx.stdout, "  auto_close: %t\n", p.AutoClose)
		}
		_, _ = fmt.Fprintf(ctx.stdout, "resolved: %s (%s)\n", resolved, source)
		return nil
	}

	selected := resolved
	if strings.TrimSpace(projectFlag) != "" {
		selected = strings.TrimSpace(projectFlag)
	}
	if selected == "" {
		_, _ = fmt.Fprintln(ctx.stdout, "No matching project resolved.")
		return nil
	}
	p, ok := cfg.Projects[selected]
	if !ok {
		return fmt.Errorf("project %q not found in config", selected)
	}

	_, _ = fmt.Fprintf(ctx.stdout, "%s\n", selected)
	_, _ = fmt.Fprintf(ctx.stdout, "  path: %s\n", p.Path)
	_, _ = fmt.Fprintf(ctx.stdout, "  store: %s\n", p.Store)
	_, _ = fmt.Fprintf(ctx.stdout, "  registered_at: %s\n", p.RegisteredAt)
	_, _ = fmt.Fprintf(ctx.stdout, "  auto_link: %t\n", p.AutoLink)
	_, _ = fmt.Fprintf(ctx.stdout, "  auto_close: %t\n", p.AutoClose)
	_, _ = fmt.Fprintf(ctx.stdout, "resolved: %s (%s)\n", resolved, source)
	return nil
}

func runConfigSet(ctx context, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: tkt config set [<project>] <field> <value>")
	}

	cfg, err := project.Load()
	if err != nil {
		return err
	}

	cwd, _ := os.Getwd()
	resolved, _ := project.ResolveName(cfg, cwd, strings.TrimSpace(ctx.projectOverride))

	projectName := ""
	field := ""
	value := ""
	if len(args) == 2 {
		if resolved == "" {
			return fmt.Errorf("no project resolved; pass explicit project name")
		}
		projectName = resolved
		field = args[0]
		value = args[1]
	} else {
		projectName = args[0]
		field = args[1]
		value = args[2]
	}

	entry, ok := cfg.Projects[projectName]
	if !ok {
		entry = project.ProjectConfig{}
	}

	switch field {
	case "path":
		abs, err := filepath.Abs(value)
		if err != nil {
			return err
		}
		entry.Path = filepath.Clean(abs)
	case "store":
		if value != "local" && value != "central" {
			return fmt.Errorf("store must be local or central")
		}
		entry.Store = value
	case "auto_link":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		entry.AutoLink = b
	case "auto_close":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		entry.AutoClose = b
	case "registered_at":
		entry.RegisteredAt = value
	default:
		return fmt.Errorf("unknown config field %q", field)
	}

	cfg.UpsertProject(projectName, entry)
	if err := project.Save(cfg); err != nil {
		return err
	}

	if ctx.json {
		return emitJSON(ctx, map[string]any{
			"project": projectName,
			"config":  entry,
		})
	}

	_, _ = fmt.Fprintf(ctx.stdout, "updated %s.%s=%s\n", projectName, field, value)
	return nil
}

func runConfigResolve(ctx context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: tkt config resolve")
	}

	cfg, err := project.Load()
	if err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	name, source := project.ResolveName(cfg, cwd, strings.TrimSpace(ctx.projectOverride))

	if ctx.json {
		return emitJSON(ctx, map[string]any{
			"project": name,
			"source":  source,
		})
	}
	if name == "" {
		_, _ = fmt.Fprintln(ctx.stdout, "No project resolved.")
		return nil
	}
	_, _ = fmt.Fprintf(ctx.stdout, "%s (%s)\n", name, source)
	return nil
}

func promptChoice(ctx context, prefix string, defaultValue string) (string, error) {
	reader := bufio.NewReader(ctx.stdin)
	_, _ = fmt.Fprint(ctx.stdout, prefix+" ")
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	value := strings.TrimSpace(line)
	if value == "" {
		return defaultValue, nil
	}
	return value, nil
}

func promptYesNo(ctx context, question string, defaultYes bool) (bool, error) {
	defaultToken := "yes"
	if !defaultYes {
		defaultToken = "no"
	}
	_, _ = fmt.Fprintf(ctx.stdout, "%s [%s]: ", question, defaultToken)
	reader := bufio.NewReader(ctx.stdin)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	value := strings.TrimSpace(strings.ToLower(line))
	if value == "" {
		return defaultYes, nil
	}
	if value == "y" || value == "yes" || value == "true" {
		return true, nil
	}
	if value == "n" || value == "no" || value == "false" {
		return false, nil
	}
	return defaultYes, nil
}

func flagProvided(args []string, flagName string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == flagName || strings.HasPrefix(arg, flagName+"=") {
			return true
		}
	}
	return false
}

func copyTicketFiles(sourceDir string, destinationDir string) error {
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(destinationDir, 0755); err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		sourcePath := filepath.Join(sourceDir, entry.Name())
		destPath := filepath.Join(destinationDir, entry.Name())

		raw, err := os.ReadFile(sourcePath)
		if err != nil {
			return err
		}
		if err := os.WriteFile(destPath, raw, 0644); err != nil {
			return err
		}
	}

	return nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
