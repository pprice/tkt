# tkt Setup

System-wide installation and per-project setup for tkt.

## Before Starting

1. **Prerequisites**: Go must be installed (`go version`).
2. **Detect context**: Check if tkt is already built, on PATH, MCP registered, etc. Skip anything that's already done.

## System Setup

### 1. Build

```bash
go build -o tkt ./cmd/tkt
```

Verify: `./tkt help` should print usage output.

### 2. Add to PATH

Symlink the built binary somewhere on PATH so `tkt` is available globally (e.g. ~/.local/bin).

Verify: `which tkt` should resolve.

### 3. Register MCP Server

tkt includes an MCP server (22 tools: 14 read, 8 write) that agents can use natively.
One-time global registration:

Claude Code:
```bash
claude mcp add --scope user --transport stdio tkt -- $(which tkt) mcp
```

Codex:
```bash
codex mcp add tkt -- $(which tkt) mcp
```

Run whichever is appropriate. MCP tools become available on the next agent session.

### 4. Add Agent Instructions

The file `internal/cli/agent-instructions.txt` in this repo contains the generic tkt
agent instructions. Append its contents to the user's agent instructions file
(`~/.claude/CLAUDE.md`, `AGENTS.md`, or equivalent).

Workflow conventions themselves are read from `~/.tkt/workflow.md` when agents run
`tkt workflow`. Users can edit that file directly to customize lifecycle states,
commit conventions, and workflow rules.

If tkt instructions are already present (look for a `## tkt` heading), skip this step.
Preserve any existing content in the file.

### 5. Start Background Service

```bash
tkt serve start
```

tkt's background service watches git commits and auto-links them to tickets. When a commit
message contains `Closes: [ticket-id]`, the ticket is automatically closed.

Verify: `tkt serve status` should show the daemon running.

## After Setup

Tell the user to start a new agent session. MCP tools and agent instructions are loaded
at session start — they won't be available until the session is restarted.

Also let them know:

1. **Ticket store backup**: If using central storage, `~/.tickets` has auto-commit but no
   remote. For backup or sync, they can add a remote.

2. **New projects**: Run `tkt init` in each new project directory.

3. **Quick start**: `tkt help`, `tkt tui`, `tkt workflow`.

4. **Custom store path**: Optionally set `TKT_ROOT=/path/to/store` to override the default `~/.tickets`
   location. Must be an absolute path.
