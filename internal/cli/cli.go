package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lawrips/tkt/internal/project"
)

type command struct {
	name        string
	aliases     []string
	usage       string
	description string
	detail      string // extended help shown by `tkt help <command>`
	run         func(ctx context, args []string) error
	subcommands map[string]command
}

type context struct {
	stdin           io.Reader
	stdout          io.Writer
	stderr          io.Writer
	json            bool
	command         string
	projectOverride string
}

var versionString = "dev"

// SetVersion sets the version string displayed by `tkt version`.
func SetVersion(v string) { versionString = v }

// Run dispatches the tkt CLI.
func Run(args []string, stdout io.Writer, stderr io.Writer) error {
	return runWithIO(args, os.Stdin, stdout, stderr)
}

func runWithIO(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	args, jsonMode, projectOverride := stripGlobalFlags(args)

	root := rootCommand()
	ctx := context{
		stdin:           stdin,
		stdout:          stdout,
		stderr:          stderr,
		json:            jsonMode,
		projectOverride: projectOverride,
	}

	if len(args) == 0 {
		printHelp(ctx, root, nil)
		return nil
	}

	if isHelpArg(args[0]) {
		printHelp(ctx, root, args[1:])
		return nil
	}

	if args[0] == "help" {
		printHelp(ctx, root, args[1:])
		return nil
	}

	cmd, consumed, err := resolveCommand(root, args)
	if err != nil {
		printHelp(ctx, root, nil)
		return err
	}
	ctx.command = strings.Join(args[:consumed], " ")

	if cmd.run == nil {
		printHelp(ctx, root, args[:consumed])
		return nil
	}

	if len(args) > consumed && isHelpArg(args[consumed]) {
		printHelp(ctx, root, args[:consumed])
		return nil
	}

	if requiresInit(args[0]) {
		if err := checkInit(projectOverride); err != nil {
			return err
		}
	}

	return cmd.run(ctx, args[consumed:])
}

func rootCommand() command {
	commands := map[string]command{
		"show":     commandWithRunner("show", "show <id>", "Display ticket details", runShow),
		"ls":       commandWithRunner("ls", "ls|list [filters]", "List tickets (default: open only)", runLS, "list"),
		"ready":    commandWithRunner("ready", "ready [filters]", "Tickets with resolved deps", runReady),
		"blocked":  commandWithRunner("blocked", "blocked [filters]", "Tickets with unresolved deps", runBlocked),
		"closed":   commandWithRunner("closed", "closed [--limit=N] [filters]", "Recently closed tickets", runClosed),
		"create":   commandWithRunner("create", "create [title] [options]", "Create ticket", runCreate),
		"edit":     commandWithRunner("edit", "edit <id> [options]", "Update ticket fields", runEdit),
		"add-note": commandWithRunner("add-note", "add-note <id> [text]", "Append timestamped note", runAddNote),
		"delete":   commandWithRunner("delete", "delete <id> [id...]", "Delete tickets", runDelete),
		"dep":      depRootCommand(),
		"undep":    commandWithRunner("undep", "undep <id> <dep-id>", "Remove dependency", runUndep),
		"link":     commandWithRunner("link", "link <id> <id> [id...]", "Create symmetric links", runLink),
		"unlink":   commandWithRunner("unlink", "unlink <id> <target-id>", "Remove link", runUnlink),
		"query":    commandWithRunner("query", "query [jq-filter]", "Output tickets as JSONL", runQuery),
		"stats":    commandWithRunner("stats", "stats", "Project health summary", runStats),
		"timeline": commandWithRunner("timeline", "timeline [--weeks=N]", "Closed tickets by week", runTimeline),
		"workflow": commandWithRunner("workflow", "workflow", "Ticket workflow guide", runWorkflow),
		"epic-view": commandWithRunner(
			"epic-view",
			"epic-view <id>",
			"Precomputed epic hierarchy with commits and deps",
			runEpicView,
		),
		"lifecycle": commandWithRunner(
			"lifecycle",
			"lifecycle <id>",
			"Ticket lifecycle: created, first/last commit, effort summary",
			runLifecycle,
		),
		"progress": commandWithRunner(
			"progress",
			"progress [--today|--week]",
			"Recent progress summary from ticket and commit journal data",
			runProgress,
		),
		"dashboard": commandWithRunner(
			"dashboard",
			"dashboard",
			"Project-level summary view",
			runDashboard,
		),
		"init": commandWithRunner("init", "init [options]", "Guided setup for storage and config", runInit),
		"migrate": commandWithRunner(
			"migrate",
			"migrate --central|--local [--yes]",
			"Move tickets between local and central storage",
			runMigrate,
		),
		"recompute": commandWithRunner(
			"recompute",
			"recompute [--yes]",
			"Rebuild commit journal from git history",
			runRecompute,
		),
		"serve": serveRootCommand(),
		"config": commandWithRunner(
			"config",
			"config [--project <name>] [--all] | config set [<project>] <field> <value> | config resolve",
			"View/edit ~/.tkt/config.yaml",
			runConfig,
		),
		"mcp": commandWithRunner(
			"mcp",
			"mcp",
			"Start MCP stdio JSON-RPC server",
			runMCP,
		),
		"version": commandWithRunner("version", "version", "Print version and exit", runVersion),
		"context": commandWithRunner(
			"context",
			"context <id>",
			"Composite view: ticket + parent + deps + linked + children + commits",
			runContext,
		),
		"tui": commandWithRunner("tui", "tui", "Interactive terminal UI", runTUI),
	}

	// Attach per-command detail text (defined in help.go).
	setDetail(commands, "create", createDetail)
	setDetail(commands, "edit", editDetail)
	setDetail(commands, "ls", lsDetail)
	setDetail(commands, "ready", readyDetail)
	setDetail(commands, "blocked", blockedDetail)
	setDetail(commands, "closed", closedDetail)
	setDetail(commands, "query", queryDetail)
	setDetail(commands, "add-note", addNoteDetail)
	setDetail(commands, "config", configDetail)
	setDetail(commands, "dep", depDetail)
	setDetail(commands, "timeline", timelineDetail)
	setDetail(commands, "init", initDetail)
	setDetail(commands, "migrate", migrateDetailFunc())
	setDetail(commands, "recompute", recomputeDetail)

	return command{
		name:        "tkt",
		usage:       "tkt <command> [args]",
		description: "ticket management CLI",
		subcommands: commands,
	}
}

func serveRootCommand() command {
	return command{
		name:        "serve",
		usage:       "serve start | serve stop | serve status | serve logs",
		description: "Manage background watcher daemon",
		subcommands: map[string]command{
			"start":  commandWithRunner("start", "serve start [--interval=5s]", "Start serve in background", runServeStart),
			"stop":   commandWithRunner("stop", "serve stop", "Stop background serve", runServeStop),
			"status": commandWithRunner("status", "serve status", "Show serve daemon status", runServeStatus),
			"logs":   commandWithRunner("logs", "serve logs [-n=50]", "Show recent serve log output", runServeLogs),
			"run":    commandWithRunner("run", "serve run [--once] [--interval=5s]", "Run watcher in foreground (internal)", runServeRun),
		},
	}
}

func depRootCommand() command {
	return command{
		name:        "dep",
		usage:       "dep <id> <dep-id> | dep tree [--full] <id> | dep cycle",
		description: "Manage dependencies",
		run:         runDep,
		subcommands: map[string]command{
			"tree":  commandWithRunner("tree", "dep tree [--full] <id>", "Show dependency tree", runDepTree),
			"cycle": commandWithRunner("cycle", "dep cycle", "Find dependency cycles", runDepCycle),
		},
	}
}

func commandWithRunner(name, usage, description string, runner func(ctx context, args []string) error, aliases ...string) command {
	return command{
		name:        name,
		aliases:     aliases,
		usage:       usage,
		description: description,
		run:         runner,
	}
}

func resolveCommand(root command, args []string) (command, int, error) {
	if len(args) == 0 {
		return root, 0, nil
	}

	current := root
	consumed := 0

	for consumed < len(args) {
		next, ok := findSubcommand(current, args[consumed])
		if !ok {
			break
		}
		current = next
		consumed++
	}

	if consumed == 0 {
		return command{}, 0, fmt.Errorf("unknown command: %s", args[0])
	}

	return current, consumed, nil
}

func findSubcommand(parent command, token string) (command, bool) {
	for _, sub := range parent.subcommands {
		if sub.name == token {
			return sub, true
		}
		for _, alias := range sub.aliases {
			if alias == token {
				return sub, true
			}
		}
	}
	return command{}, false
}

// printHelp and sortedSubcommands live in help.go.

func isHelpArg(token string) bool {
	return token == "-h" || token == "--help" || strings.EqualFold(token, "help")
}

// requiresInit returns true for commands that need a resolved project.
func requiresInit(cmdName string) bool {
	switch cmdName {
	case "init", "config", "tui", "mcp", "serve", "workflow", "version":
		return false
	}
	return true
}

// checkInit verifies the current directory resolves to a registered project.
func checkInit(projectOverride string) error {
	cfg, err := project.Load()
	if err != nil {
		return fmt.Errorf("tkt not initialized for this directory. Run 'tkt init' to set up.")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	name, _ := project.ResolveName(cfg, cwd, projectOverride)
	if name == "" {
		return fmt.Errorf("tkt not initialized for this directory. Run 'tkt init' to set up.")
	}
	if _, ok := cfg.Projects[name]; !ok {
		return fmt.Errorf("tkt not initialized for this directory. Run 'tkt init' to set up.")
	}
	return nil
}

func stripGlobalFlags(args []string) ([]string, bool, string) {
	clean := make([]string, 0, len(args))
	jsonMode := false
	project := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--json" {
			jsonMode = true
			continue
		}
		if strings.HasPrefix(arg, "--project=") {
			project = strings.TrimPrefix(arg, "--project=")
			continue
		}
		if arg == "--project" {
			if i+1 < len(args) {
				project = args[i+1]
				i++
				continue
			}
		}
		clean = append(clean, arg)
	}
	return clean, jsonMode, project
}
