package cli

import (
	"fmt"
	"sort"
)

// helpSection groups commands under a heading for the root help output.
type helpSection struct {
	title   string
	entries []helpEntry
}

type helpEntry struct {
	usage       string
	description string
}

func rootHelpSections() []helpSection {
	return []helpSection{
		{
			title: "Viewing",
			entries: []helpEntry{
				{"show <id>", "Display ticket details"},
				{"ls|list [search] [filters]", "List tickets (default: open only)"},
				{"ready [filters]", "Tickets with resolved deps"},
				{"blocked [filters]", "Tickets with unresolved deps"},
				{"closed [--limit=N]", "Recently closed (default limit: 20)"},
			},
		},
		{
			title: "Creating & Editing",
			entries: []helpEntry{
				{"create [title] [options]", "Create ticket"},
				{"edit <id> [options]", "Update ticket fields"},
				{"add-note <id> [text]", "Append timestamped note (stdin if no text)"},
				{"delete <id> [id...]", "Delete ticket(s)"},
			},
		},
		{
			title: "Dependencies & Links",
			entries: []helpEntry{
				{"dep <id> <dep-id>", "Add dependency (id depends on dep-id)"},
				{"undep <id> <dep-id>", "Remove dependency"},
				{"dep tree [--full] <id>", "Show dependency tree"},
				{"dep cycle", "Find cycles in open tickets"},
				{"link <id> <id> [id...]", "Create symmetric links"},
				{"unlink <id> <target-id>", "Remove link"},
			},
		},
		{
			title: "Query (JSON)",
			entries: []helpEntry{
				{"query [jq-filter]", "Output all tickets as JSONL"},
			},
		},
		{
			title: "Analytics & Views",
			entries: []helpEntry{
				{"stats", "Project health summary"},
				{"timeline [--weeks=N]", "Closed tickets by week"},
				{"lifecycle <id>", "Ticket lifecycle and effort summary"},
				{"progress [--today|--week]", "Recent progress summary"},
				{"dashboard", "Project-level summary view"},
				{"epic-view <id>", "Epic hierarchy with commits and deps"},
				{"context <id>", "Ticket + parent + deps + linked + children + commits"},
			},
		},
		{
			title: "Workflow & Tools",
			entries: []helpEntry{
				{"workflow", "Ticket lifecycle, commit format, and conventions  ★"},
				{"tui", "Interactive terminal UI"},
				{"serve start|stop|status|logs", "Manage background watcher daemon"},
				{"mcp", "Start MCP stdio JSON-RPC server"},
			},
		},
		{
			title: "Configuration",
			entries: []helpEntry{
				{"init [options]", "Guided setup for storage and config"},
				{"config", "View/edit ~/.tkt/config.yaml"},
				{"migrate --central|--local", "Move tickets between storage modes"},
				{"recompute [--yes]", "Rebuild commit journal from git history"},
				{"version", "Print version and exit"},
			},
		},
	}
}

func printHelp(ctx context, root command, path []string) {
	target := root

	for _, token := range path {
		next, ok := findSubcommand(target, token)
		if !ok {
			fmt.Fprintf(ctx.stderr, "unknown command in help path: %s\n", token)
			return
		}
		target = next
	}

	if target.name == root.name {
		printRootHelp(ctx)
		return
	}

	// Command-specific help.
	fmt.Fprintf(ctx.stdout, "Usage:\n  tkt %s\n\n", target.usage)
	fmt.Fprintf(ctx.stdout, "%s\n", target.description)

	if len(target.subcommands) > 0 && target.detail == "" {
		fmt.Fprintln(ctx.stdout)
		fmt.Fprintln(ctx.stdout, "Subcommands:")
		for _, key := range sortedSubcommands(target) {
			sub := target.subcommands[key]
			fmt.Fprintf(ctx.stdout, "  %-12s %s\n", sub.name, sub.description)
		}
	}

	if target.detail != "" {
		fmt.Fprintln(ctx.stdout)
		fmt.Fprintln(ctx.stdout, target.detail)
	}
}

func printRootHelp(ctx context) {
	fmt.Fprintln(ctx.stdout, "tkt - ticket management CLI")
	fmt.Fprintln(ctx.stdout)
	fmt.Fprintln(ctx.stdout, "Usage: tkt <command> [args]")

	for _, section := range rootHelpSections() {
		fmt.Fprintln(ctx.stdout)
		fmt.Fprintf(ctx.stdout, "%s:\n", section.title)
		for _, entry := range section.entries {
			fmt.Fprintf(ctx.stdout, "  %-27s %s\n", entry.usage, entry.description)
		}
	}

	fmt.Fprintln(ctx.stdout)
	fmt.Fprintln(ctx.stdout, "Global flags:")
	fmt.Fprintf(ctx.stdout, "  %-27s %s\n", "--json", "Output as JSON (supported by most commands)")
	fmt.Fprintf(ctx.stdout, "  %-27s %s\n", "--project <name>", "Override auto-detected project")

	fmt.Fprintln(ctx.stdout)
	fmt.Fprintln(ctx.stdout, "Partial ID matching: 'tkt show 5c4' matches 'nw-5c46'")
	fmt.Fprintln(ctx.stdout, "Tickets stored as markdown in .tickets/ or ~/.tickets/<project>/")
	fmt.Fprintln(ctx.stdout)
	fmt.Fprintln(ctx.stdout, "Use `tkt help <command>` for details.")
}

// Per-command detail strings.

const createDetail = `Options:
  -d, --description <text>   Description text
  --design <text>            Design notes
  --acceptance <text>        Acceptance criteria
  -t, --type <type>          bug | feature | task | epic | chore [default: task]
  -p, --priority <n>         0-4, 0=highest [default: 2]
  -a, --assignee <name>      Assignee
  --id <id>                  Custom ticket ID (e.g., --id my-feature)
  --parent <id>              Parent ticket ID
  --tags <tags>              Comma-separated (e.g., --tags ui,backend)
  --external-ref <ref>       External reference (e.g., gh-123)

Examples:
  tkt create "Fix login bug" -t bug -p 1
  tkt create "Add dark mode" --id ui-dark-mode -t feature
  tkt create "Refactor auth" --parent my-epic --tags backend,auth`

const editDetail = `Options:
  --title <text>             New title
  -d, --description <text>   Description text
  --design <text>            Design notes
  --acceptance <text>        Acceptance criteria
  -t, --type <type>          bug | feature | task | epic | chore
  -p, --priority <n>         0-4, 0=highest
  -s, --status <status>      open | in_progress | needs_testing | closed
  -a, --assignee <name>      Assignee
  --parent <id>              Parent ticket ID
  --tags <tags>              Comma-separated (e.g., --tags ui,backend)
  --external-ref <ref>       External reference (e.g., gh-123)

Examples:
  tkt edit my-ticket -s in_progress
  tkt edit my-ticket -p 0 --tags urgent
  tkt edit my-ticket --title "Updated title"`

const lsDetail = `Filter flags:
  --status <status>          open | in_progress | needs_testing | closed
  -t, --type <type>          bug | feature | task | epic | chore
  -P, --priority <n>         0 (critical) through 4 (backlog)
  -a, --assignee <name>      Filter by assignee
  -T, --tag <tag>            Filter by tag
  --parent <id>              Children of ticket <id>
  --sort <field>             Sort by: id, created, modified, priority, title
                             Append :desc for descending (default: asc)
  --limit <n>                Max tickets to show
  --search <query>           Search ID and title (case-insensitive)

Text search (positional arg, matches ID and title):
  tkt ls "websocket"          Search open tickets
  tkt ls -t bug "websocket"   Search open bugs

Examples:
  tkt ls                      All open tickets
  tkt ls -t bug               Open bugs
  tkt ls --status in_progress In-progress tickets
  tkt ls --parent my-epic     Children of an epic
  tkt ls -P 0                 Critical priority
  tkt ls --sort created:desc      Newest first
  tkt ls --sort priority --limit 5  Top 5 by priority`

const readyDetail = `Filter flags:
  -a, --assignee <name>      Filter by assignee
  -T, --tag <tag>            Filter by tag
  --open                     Skip parent hierarchy checks

Shows open tickets whose dependencies are all resolved and whose
parent (if any) is in_progress. Use --open to skip parent checks.`

const blockedDetail = `Filter flags:
  -a, --assignee <name>      Filter by assignee
  -T, --tag <tag>            Filter by tag

Shows open tickets that have at least one unresolved dependency.`

const closedDetail = `Filter flags:
  --limit <n>                Max tickets to show [default: 20]
  --sort <field>             Sort by: id, created, modified, priority, title
                             Append :desc for descending (default: asc)
  -a, --assignee <name>      Filter by assignee
  -T, --tag <tag>            Filter by tag`

const queryDetail = `The optional filter is passed to jq's select() automatically.
Do NOT wrap your filter in select() — just provide the expression.
Use single quotes for the filter to avoid shell issues with ! and ".

Examples:
  tkt query                                        All tickets as JSONL
  tkt query '.status == "open"'                    Filter by field
  tkt query '.type == "bug" and .priority <= 1'    Compound filter
  tkt query '.title | test("deploy"; "i")'         Regex search

JSON fields: id, status, type, priority, title, description,
  design, acceptance_criteria, deps[], links[], tags[],
  created, assignee, parent, notes, external_ref
Body sections (## Heading) become snake_case fields.`

const addNoteDetail = `Appends a timestamped note to the ticket's Notes section.
If no text argument is given, reads from stdin.

Examples:
  tkt add-note my-ticket "Discussed in standup, deprioritized"
  echo "Long note content" | tkt add-note my-ticket`

const configDetail = `Subcommands:
  tkt config                           Show config for current project
  tkt config --all                     Show full config file
  tkt config --project <name>          Show config for specific project
  tkt config set <field> <value>       Set field for current project
  tkt config set <project> <field> <value>  Set field for specific project
  tkt config resolve                   Show resolved ticket directory`

const depDetail = `Subcommands:
  tkt dep <id> <dep-id>       Add dependency (id depends on dep-id)
  tkt dep tree [--full] <id>  Show dependency tree (--full includes closed)
  tkt dep cycle               Find dependency cycles in open tickets

Examples:
  tkt dep my-task prerequisite-task
  tkt dep tree my-epic
  tkt dep tree --full my-epic`

const timelineDetail = `Options:
  --weeks <n>                Number of weeks to show [default: 4]

Displays a histogram of tickets closed per week.`

const initDetail = `Options:
  --project <name>           Project name [default: git repo name]
  --store <mode>             central | local [default: prompted]
  --auto-link=true|false     Auto-link commits to tickets [default: true]
  --auto-close=true|false    Auto-close tickets on closing commits [default: true]
  --yes                      Skip confirmation prompts

Sets up tkt for the current repository: validates git state, configures
storage mode and project mapping.`

const migrateDetail = `Options:
  --central                  Move tickets to central store (~/.tickets/<project>/)
  --local                    Move tickets to local .tickets/ directory
  --yes                      Skip confirmation prompt

Exactly one of --central or --local is required.`

const recomputeDetail = `Options:
  --yes                      Skip confirmation prompt

Rebuilds the commit journal from git log. Useful after initial
setup or if the journal gets out of sync with git history.`

func setDetail(commands map[string]command, name, detail string) {
	cmd := commands[name]
	cmd.detail = detail
	commands[name] = cmd
}

func sortedSubcommands(parent command) []string {
	keys := make([]string, 0, len(parent.subcommands))
	for key := range parent.subcommands {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
