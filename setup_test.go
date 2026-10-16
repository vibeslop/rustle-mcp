package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Stand-ins for the Claude Code and Codex CLIs, which keep the 'rustle' server's command
// line in a file. Like the real ones, claude refuses a name it already has and codex
// replaces it.
const fakeClaude = `#!/bin/sh
state="$FAKE_STATE/claude"
case "$1 $2" in
"mcp add")
  [ -f "$state" ] && { echo "MCP server rustle already exists in user config" >&2; exit 1; }
  shift 2; while [ "$1" != "--" ]; do shift; done; shift
  echo "$*" > "$state" ;;
"mcp get")
  [ -f "$state" ] || { echo "No MCP server found with name: rustle" >&2; exit 1; }
  set -- $(/bin/cat "$state"); command=$1; shift
  printf 'rustle:\n  Scope: User config\n  Command: %s\n  Args: %s\n' "$command" "$*" ;;
"mcp remove")
  /bin/rm -f "$state" ;;
esac
`

const fakeCodex = `#!/bin/sh
state="$FAKE_STATE/codex"
case "$1 $2" in
"mcp add")
  shift 2; while [ "$1" != "--" ]; do shift; done; shift
  echo "$*" > "$state"
  echo "Added global MCP server 'rustle'." ;;
"mcp get")
  [ -f "$state" ] || exit 1
  set -- $(/bin/cat "$state"); command=$1; shift
  printf 'rustle\n  enabled: true\n  command: %s\n  args: %s\n' "$command" "${*:--}" ;;
esac
`

const server = "/opt/rustle/rustle-mcp"

// sandbox is a home folder and a PATH holding only the clients named, so a test never
// reaches the real ones or their configs.
func sandbox(t *testing.T, clis ...string) (home, state string) {
	t.Helper()
	root := t.TempDir()
	home, state = filepath.Join(root, "home"), filepath.Join(root, "state")
	bin := filepath.Join(root, "bin")
	for _, dir := range []string{home, state, bin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range clis {
		script := map[string]string{"claude": fakeClaude, "codex": fakeCodex}[name]
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("FAKE_STATE", state)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude-config"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex-config"))
	return home, state
}

func runSetup(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	code := setupWith(args, &out, func() (string, error) { return server, nil },
		func() (io.ReadWriteCloser, error) { return nil, errors.New("no terminal") })
	return out.String(), code
}

// keyboard is a terminal whose user presses keys, one read each, then closes it.
type keyboard struct {
	presses []string
	screen  bytes.Buffer
}

func (k *keyboard) Read(p []byte) (int, error) {
	if len(k.presses) == 0 {
		return 0, io.EOF
	}
	n := copy(p, k.presses[0])
	k.presses = k.presses[1:]
	return n, nil
}

func (k *keyboard) Write(p []byte) (int, error) { return k.screen.Write(p) }
func (k *keyboard) Close() error                { return nil }

// runSetupAt runs setup as from a terminal, where its user presses keys.
func runSetupAt(t *testing.T, presses []string, args ...string) (string, *keyboard) {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	var out bytes.Buffer
	k := &keyboard{presses: presses}
	setupWith(args, &out, func() (string, error) { return server, nil },
		func() (io.ReadWriteCloser, error) { return k, nil })
	return out.String(), k
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func needShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in CLIs are shell scripts")
	}
}

func TestSetupAddsRustleToEveryClientFound(t *testing.T) {
	needShell(t)
	home, state := sandbox(t, "claude", "codex")
	// Cursor's list links into a dotfiles folder and holds another server.
	cursor := filepath.Join(home, ".cursor", "mcp.json")
	dotfile := filepath.Join(home, "dotfiles", "mcp.json")
	os.MkdirAll(filepath.Dir(cursor), 0o755)
	os.MkdirAll(filepath.Dir(dotfile), 0o755)
	os.WriteFile(dotfile, []byte(`{"mcpServers":{"other":{"command":"other-server","args":["--x"]}},"theme":"dark"}`), 0o644)
	if err := os.Symlink(dotfile, cursor); err != nil {
		t.Fatal(err)
	}

	out, code := runSetup(t)
	want := "Added Rustle to Claude Code.\nAdded Rustle to Codex.\nAdded Rustle to Cursor.\n" +
		"Restart Claude Code, Codex and Cursor, or reconnect their MCP servers, to start using it.\n"
	if code != 0 || out != want {
		t.Fatalf("setup = %d\n%s\nwant\n%s", code, out, want)
	}
	for _, client := range []string{"claude", "codex"} {
		if got := strings.TrimSpace(read(t, filepath.Join(state, client))); got != server {
			t.Errorf("%s runs %q", client, got)
		}
	}
	if info, err := os.Lstat(cursor); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Error("Cursor's link was replaced by a file")
	}
	wantCursor := `{
  "mcpServers": {
    "other": {
      "command": "other-server",
      "args": [
        "--x"
      ]
    },
    "rustle": {
      "command": "/opt/rustle/rustle-mcp",
      "args": []
    }
  },
  "theme": "dark"
}
`
	if got := read(t, dotfile); got != wantCursor {
		t.Errorf("Cursor's list:\n%s\nwant\n%s", got, wantCursor)
	}

	// Running it again is how an install updates.
	out, _ = runSetup(t)
	want = "Updated Rustle in Claude Code.\nUpdated Rustle in Codex.\nUpdated Rustle in Cursor.\n" +
		"Restart Claude Code, Codex and Cursor, or reconnect their MCP servers, to start using it.\n"
	if out != want {
		t.Errorf("again:\n%s\nwant\n%s", out, want)
	}
}

func TestSetupReplacesAnEarlierRustle(t *testing.T) {
	needShell(t)
	home, state := sandbox(t, "claude", "codex")
	os.WriteFile(filepath.Join(state, "claude"), []byte("node /old/dist/rustle.mjs\n"), 0o644)
	os.WriteFile(filepath.Join(state, "codex"), []byte("uv run rustle-mcp\n"), 0o644)
	os.MkdirAll(filepath.Join(home, ".cursor"), 0o755)
	os.WriteFile(filepath.Join(home, ".cursor", "mcp.json"), []byte(`{"mcpServers":{"rustle":{"command":"node","args":["/old/rustle.mjs"]}}}`), 0o644)

	out, code := runSetup(t)
	want := "Added Rustle to Claude Code, replacing the 'rustle' server that ran node /old/dist/rustle.mjs.\n" +
		"Added Rustle to Codex, replacing the 'rustle' server that ran uv run rustle-mcp.\n" +
		"Added Rustle to Cursor, replacing the 'rustle' server that ran node /old/rustle.mjs.\n" +
		"Restart Claude Code, Codex and Cursor, or reconnect their MCP servers, to start using it.\n"
	if code != 0 || out != want {
		t.Fatalf("setup = %d\n%s\nwant\n%s", code, out, want)
	}
	if got := strings.TrimSpace(read(t, filepath.Join(state, "claude"))); got != server {
		t.Errorf("Claude Code runs %q", got)
	}
}

func TestSetupOfOneClientLeavesTheOthers(t *testing.T) {
	needShell(t)
	_, state := sandbox(t, "claude", "codex")
	out, code := runSetup(t, "codex")
	if code != 0 || out != "Added Rustle to Codex.\nRestart Codex, or reconnect its MCP servers, to start using it.\n" {
		t.Errorf("setup codex = %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(state, "claude")); err == nil {
		t.Error("Claude Code was changed too")
	}
	// A client named but not installed says how to add Rustle by hand, and fails.
	out, code = runSetup(t, "cursor")
	if code != 1 || !strings.HasPrefix(out, "Cursor isn't installed here. To add Rustle by hand:\n  Open cursor://") {
		t.Errorf("setup cursor = %d\n%s", code, out)
	}
}

func TestSetupWithNoClientPrintsTheConfig(t *testing.T) {
	sandbox(t)
	out, code := runSetup(t)
	want := "Found no Claude Code, Codex or Cursor here. To use Rustle from another client, add this to its MCP servers:\n" +
		"{\n  \"mcpServers\": {\n    \"rustle\": {\n      \"command\": \"/opt/rustle/rustle-mcp\",\n      \"args\": []\n    }\n  }\n}\n"
	if code != 0 || out != want {
		t.Errorf("setup = %d\n%s\nwant\n%s", code, out, want)
	}
	if out, _ := runSetup(t, "json"); !strings.HasPrefix(out, "{\n  \"mcpServers\"") {
		t.Errorf("setup json:\n%s", out)
	}
}

func TestSetupLeavesACursorListItCantReadAlone(t *testing.T) {
	home, _ := sandbox(t)
	cursor := filepath.Join(home, ".cursor", "mcp.json")
	os.MkdirAll(filepath.Dir(cursor), 0o755)
	commented := "{\n  // my servers\n  \"mcpServers\": {}\n}\n"
	os.WriteFile(cursor, []byte(commented), 0o644)
	out, code := runSetup(t)
	if code != 1 || !strings.Contains(out, "isn't plain JSON, so it was left as it is") {
		t.Errorf("setup = %d\n%s", code, out)
	}
	if read(t, cursor) != commented {
		t.Error("the list was rewritten")
	}
}

func TestSetupArguments(t *testing.T) {
	needShell(t)
	sandbox(t, "claude")
	out, _ := runSetup(t, "claude", "--print", "--scope", "project", "--media-roots", "/a", "/b")
	want := "Claude Code:\n  claude mcp add --scope project --env RUSTLE_MEDIA_ROOTS=/a:/b rustle -- /opt/rustle/rustle-mcp\n"
	if out != want {
		t.Errorf("--print:\n%s\nwant\n%s", out, want)
	}
	if _, code := runSetup(t, "vscode"); code != 2 {
		t.Errorf("an unknown client exits %d", code)
	}
}

func TestSetupAsksWhichClientsAtATerminal(t *testing.T) {
	needShell(t)
	_, state := sandbox(t, "claude", "codex")
	// Move to Codex, tick it, confirm.
	out, k := runSetupAt(t, []string{"\x1b[B", " ", "\r"})
	if want := "Added Rustle to Codex.\nRestart Codex, or reconnect its MCP servers, to start using it.\n"; out != want {
		t.Errorf("setup:\n%s\nwant\n%s", out, want)
	}
	if _, err := os.Stat(filepath.Join(state, "claude")); err == nil {
		t.Error("Claude Code was added though never ticked")
	}
	screen := k.screen.String()
	for _, frame := range []string{
		"Add Rustle to:\n› ○ Claude Code\n  ○ Codex\n  ↑↓ move · space toggle · enter confirm\n",
		"Add Rustle to:\n  ○ Claude Code\n› ● Codex\n",
	} {
		if !strings.Contains(screen, frame) {
			t.Errorf("never drew\n%s\nin\n%q", frame, screen)
		}
	}
	// The list is erased and the cursor shown again.
	if !strings.HasSuffix(screen, "\x1b[4A\x1b[J\x1b[?25h") {
		t.Errorf("left the screen as %q", screen)
	}

	// Arrows move between clients, wrapping around.
	out, _ = runSetupAt(t, []string{"\x1b[A", " ", "\r"})
	if want := "Updated Rustle in Codex.\nRestart Codex, or reconnect its MCP servers, to start using it.\n"; out != want {
		t.Errorf("setup:\n%s\nwant\n%s", out, want)
	}
}

func TestSetupAtATerminalCanAddNothing(t *testing.T) {
	home, _ := sandbox(t)
	os.MkdirAll(filepath.Join(home, ".cursor"), 0o755)
	// Escape, Ctrl-C, confirming with nothing ticked, and the terminal closing all leave the
	// clients alone.
	for _, presses := range [][]string{{"\x1b"}, {"\x03"}, {"\r"}, {" ", " ", "\r"}, {}} {
		out, _ := runSetupAt(t, presses)
		if want := "No client was changed. To add Rustle to one later, run /opt/rustle/rustle-mcp setup\n"; out != want {
			t.Errorf("keys %q:\n%s\nwant\n%s", presses, out, want)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".cursor", "mcp.json")); err == nil {
		t.Error("Cursor's list was written")
	}
	if out, _ := runSetupAt(t, []string{" ", "\r"}); out != "Added Rustle to Cursor.\nRestart Cursor, or reconnect its MCP servers, to start using it.\n" {
		t.Errorf("enter:\n%s", out)
	}
}

func TestSetupDoesntAskWhenClientsAreNamed(t *testing.T) {
	home, _ := sandbox(t)
	os.MkdirAll(filepath.Join(home, ".cursor"), 0o755)
	for _, args := range [][]string{{"cursor"}, {"--print"}} {
		if _, k := runSetupAt(t, []string{"\x1b"}, args...); k.screen.Len() > 0 {
			t.Errorf("setup %v asked:\n%q", args, k.screen.String())
		}
	}
}

func TestKeys(t *testing.T) {
	got := keys([]byte("\x1b[A\x1bOB \r\x1b[1;5Cq\x1b"))
	want := []string{"\x1b[A", "\x1bOB", " ", "\r", "\x1b[1;5C", "q", "\x1b"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("keys = %q, want %q", got, want)
	}
}
