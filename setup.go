package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/vibeslop/rustle-mcp/internal/jsonv"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
	"github.com/vibeslop/rustle-mcp/internal/source"
	"github.com/vibeslop/rustle-mcp/internal/term"
)

const name = "rustle"

// registration is what a client is told to run.
type registration struct {
	command string
	env     map[string]string
	// scope is Claude Code's: user, project or local.
	scope string
}

func (r registration) envFlags() []string {
	var flags []string
	for k, v := range r.env {
		flags = append(flags, "--env", k+"="+v)
	}
	return flags
}

// entry is the server as JSON configs list it, command first.
type entry struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
}

func (r registration) entry() entry { return entry{Command: r.command, Args: []string{}, Env: r.env} }

// client is an app Rustle can add itself to.
type client struct {
	id, label string
	present   func() bool
	register  func(registration) (string, error)
	// manual is what to run or paste to add Rustle by hand.
	manual func(registration) string
}

var clients = []client{
	{id: "claude", label: "Claude Code", present: func() bool { return cli("claude") != "" }, register: registerClaude, manual: func(r registration) string {
		return quote(append(append([]string{"claude", "mcp", "add", "--scope", r.scope}, r.envFlags()...), name, "--", r.command))
	}},
	{id: "codex", label: "Codex", present: func() bool { return cli("codex") != "" }, register: registerCodex, manual: func(r registration) string {
		return quote(append(append([]string{"codex", "mcp", "add"}, r.envFlags()...), name, "--", r.command))
	}},
	{id: "cursor", label: "Cursor", present: cursorPresent, register: registerCursor, manual: func(r registration) string {
		data, _ := json.Marshal(r.entry())
		return "Open cursor://anysphere.cursor-deeplink/mcp/install?name=" + name + "&config=" + base64.StdEncoding.EncodeToString(data) +
			"\nor add this to " + cursorConfig() + ":\n" + jsonConfig(r)
	}},
}

// cli is a client's command line tool on PATH, "" when it isn't installed. Setup runs
// from a terminal, whose PATH finds what its user runs.
func cli(command string) string {
	path, _ := exec.LookPath(command)
	return path
}

// lastLine is a tool's last line of output, which says what went wrong.
func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// added says what registering did, given the command line the client ran before ("" when
// it had no 'rustle' server). Running setup again is how an install updates.
func added(label, before string, r registration) string {
	switch {
	case before == "":
		return "Added Rustle to " + label + "."
	case before == r.command:
		return "Updated Rustle in " + label + "."
	}
	return "Added Rustle to " + label + ", replacing the 'rustle' server that ran " + before + "."
}

// listed reads a command line from a CLI's description of a server: its command and args
// fields, whatever their case.
func listed(description string) string {
	var command, args string
	for _, line := range strings.Split(description, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		value = strings.TrimSpace(value)
		switch strings.ToLower(key) {
		case "command":
			if ok {
				command = value
			}
		case "args":
			if ok && value != "-" {
				args = value
			}
		}
	}
	return strings.TrimSpace(command + " " + args)
}

// registerClaude adds the server to Claude Code. Its CLI refuses a name it already has, so
// an earlier entry, most often an older install, is replaced.
func registerClaude(r registration) (string, error) {
	claude := cli("claude")
	add := append(append([]string{"mcp", "add", "--scope", r.scope}, r.envFlags()...), name, "--", r.command)
	out, err := exec.Command(claude, add...).CombinedOutput()
	if err == nil {
		return added("Claude Code", "", r), nil
	}
	if !strings.Contains(string(out), "already exists") {
		return "", errors.New(lastLine(out))
	}
	description, _ := exec.Command(claude, "mcp", "get", name).Output()
	if out, err := exec.Command(claude, "mcp", "remove", name, "--scope", r.scope).CombinedOutput(); err != nil {
		return "", errors.New(lastLine(out))
	}
	if out, err := exec.Command(claude, add...).CombinedOutput(); err != nil {
		return "", errors.New(lastLine(out))
	}
	return added("Claude Code", listed(string(description)), r), nil
}

// registerCodex adds the server to Codex, whose CLI replaces an entry of the same name.
func registerCodex(r registration) (string, error) {
	codex := cli("codex")
	before := ""
	if description, err := exec.Command(codex, "mcp", "get", name).Output(); err == nil {
		before = listed(string(description))
	}
	add := append(append([]string{"mcp", "add"}, r.envFlags()...), name, "--", r.command)
	if out, err := exec.Command(codex, add...).CombinedOutput(); err != nil {
		return "", errors.New(lastLine(out))
	}
	return added("Codex", before, r), nil
}

func cursorConfig() string { return filepath.Join(rustle.Home(), ".cursor", "mcp.json") }

// cursorPresent: Cursor makes its folder in home on first start.
func cursorPresent() bool {
	info, err := os.Stat(filepath.Join(rustle.Home(), ".cursor"))
	return err == nil && info.IsDir()
}

// registerCursor adds the server to Cursor's own list, keeping every other server and the
// file's order. A config that isn't plain JSON is left alone rather than rewritten.
func registerCursor(r registration) (string, error) {
	path := cursorConfig()
	// The file may link into a dotfiles folder: write where it points, and keep the link.
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	root := &jsonv.Object{}
	mode := os.FileMode(0o644)
	data, err := os.ReadFile(path)
	switch {
	case err == nil && len(bytes.TrimSpace(data)) > 0:
		parsed, perr := jsonv.Parse(data)
		if root = jsonv.Obj(parsed); perr != nil || root == nil {
			return "", fmt.Errorf("%s isn't plain JSON, so it was left as it is", source.DisplayPath(cursorConfig()))
		}
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	servers := jsonv.Obj(root.Get("mcpServers"))
	if servers == nil {
		servers = &jsonv.Object{}
		set(root, "mcpServers", servers)
	}
	before := ""
	if old := jsonv.Obj(servers.Get(name)); old != nil {
		before = jsonv.String(old.Get("command"))
		for _, arg := range jsonv.Arr(old.Get("args")) {
			before += " " + jsonv.String(arg)
		}
	}
	set(servers, name, r.entry())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := rustle.WriteAtomic(path, append(indented(nil, root, ""), '\n')); err != nil {
		return "", err
	}
	if err := os.Chmod(path, mode); err != nil {
		return "", err
	}
	return added("Cursor", before, r), nil
}

// set adds or replaces a key, keeping its place.
func set(o *jsonv.Object, key string, value any) {
	for i, k := range o.Keys {
		if k == key {
			o.Values[i] = value
			return
		}
	}
	o.Keys = append(o.Keys, key)
	o.Values = append(o.Values, value)
}

// indented writes a JSON value with two-space indents, objects in their own order.
func indented(out []byte, v any, indent string) []byte {
	inner := indent + "  "
	switch x := v.(type) {
	case *jsonv.Object:
		if len(x.Keys) == 0 {
			return append(out, "{}"...)
		}
		out = append(out, "{\n"...)
		for i, k := range x.Keys {
			out = append(out, inner...)
			out = rustle.AppendString(out, k)
			out = append(out, ": "...)
			out = indented(out, x.Values[i], inner)
			if i < len(x.Keys)-1 {
				out = append(out, ',')
			}
			out = append(out, '\n')
		}
		return append(append(out, indent...), '}')
	case []any:
		if len(x) == 0 {
			return append(out, "[]"...)
		}
		out = append(out, "[\n"...)
		for i, item := range x {
			out = append(out, inner...)
			out = indented(out, item, inner)
			if i < len(x)-1 {
				out = append(out, ',')
			}
			out = append(out, '\n')
		}
		return append(append(out, indent...), ']')
	case float64:
		return rustle.AppendNumber(out, x)
	case string:
		return rustle.AppendString(out, x)
	case bool:
		if x {
			return append(out, "true"...)
		}
		return append(out, "false"...)
	case nil:
		return append(out, "null"...)
	default:
		// Values Rustle adds itself: its entry's maps and lists.
		data, _ := json.Marshal(x)
		parsed, _ := jsonv.Parse(data)
		return indented(out, parsed, indent)
	}
}

func jsonConfig(r registration) string {
	data, _ := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{name: r.entry()}}, "", "  ")
	return string(data)
}

// binary is this executable, from where it will stay. Built by go run it sits in a
// go-build folder that is deleted when it exits, so it is copied to a folder of its own
// first: the server is one self-contained file. Anywhere else, it is where its user put it.
func binary() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}
	if !strings.Contains(self, string(filepath.Separator)+"go-build") {
		return self, nil
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(rustle.Home(), ".local", "share")
	}
	if runtime.GOOS == "windows" {
		if data = os.Getenv("LOCALAPPDATA"); data == "" {
			data = filepath.Join(rustle.Home(), "AppData", "Local")
		}
	}
	dir := filepath.Join(data, "rustle-mcp")
	target := filepath.Join(dir, filepath.Base(self))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := copyFile(self, target); err != nil {
		return "", err
	}
	fmt.Printf("Copied the server to %s\n", target)
	return target, nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	temporary := to + ".tmp"
	out, err := os.OpenFile(temporary, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, to)
}

var plain = regexp.MustCompile(`^[\w./:=@-]+$`)

func quote(parts []string) string {
	out := make([]string, len(parts))
	for i, p := range parts {
		if plain.MatchString(p) {
			out[i] = p
		} else {
			out[i] = "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
		}
	}
	return strings.Join(out, " ")
}

// setup adds Rustle to the clients named, or to the installed ones its user ticks, or to
// every one installed when nobody is at a terminal to ask. Running it again is how an
// install updates: the entries already run this binary.
func setup(argv []string) int { return setupWith(argv, os.Stdout, binary, term.Open) }

// setupWith is setup writing to out, with locate naming the binary clients should run and
// terminal opening the one to ask on.
func setupWith(argv []string, out io.Writer, locate func() (string, error), terminal func() (io.ReadWriteCloser, error)) int {
	var named []string
	printOnly := false
	r := registration{scope: "user"}
	var roots []string
	for i := 0; i < len(argv); i++ {
		switch a := argv[i]; {
		case a == "--print":
			printOnly = true
		case a == "--scope" && i+1 < len(argv):
			i++
			r.scope = argv[i]
		case a == "--media-roots":
			// Folders run until the next flag.
			for i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "--") {
				i++
				if abs, err := filepath.Abs(argv[i]); err == nil {
					roots = append(roots, abs)
				}
			}
		case a == "json" || find(a) != nil:
			named = append(named, a)
		default:
			fmt.Fprintf(os.Stderr, "rustle-mcp setup: unknown argument %q; clients are claude, codex, cursor and json\n", a)
			return 2
		}
	}
	if len(roots) > 0 {
		r.env = map[string]string{"RUSTLE_MEDIA_ROOTS": strings.Join(roots, rustle.PathListSeparator)}
	}
	command, err := locate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rustle-mcp setup: %v\n", err)
		return 1
	}
	r.command = command
	if len(named) == 1 && named[0] == "json" {
		fmt.Fprintln(out, jsonConfig(r))
		return 0
	}
	chosen := named
	if len(chosen) == 0 {
		var labels []string
		for _, c := range clients {
			if printOnly || c.present() {
				chosen = append(chosen, c.id)
				labels = append(labels, c.label)
			}
		}
		if len(chosen) == 0 {
			fmt.Fprintf(out, "Found no Claude Code, Codex or Cursor here. To use Rustle from another client, add this to its MCP servers:\n%s\n", jsonConfig(r))
			return 0
		}
		if !printOnly {
			if t, err := terminal(); err == nil {
				ticked := checklist(t, "Add Rustle to:", labels)
				t.Close()
				found := chosen
				chosen = nil
				for _, i := range ticked {
					chosen = append(chosen, found[i])
				}
				if len(chosen) == 0 {
					fmt.Fprintf(out, "No client was changed. To add Rustle to one later, run %s\n", quote([]string{r.command, "setup"}))
					return 0
				}
			}
		}
	}
	failed := 0
	var done []string
	for _, id := range chosen {
		if id == "json" {
			fmt.Fprintln(out, jsonConfig(r))
			continue
		}
		c := find(id)
		if printOnly {
			fmt.Fprintf(out, "%s:\n  %s\n", c.label, strings.ReplaceAll(c.manual(r), "\n", "\n  "))
			continue
		}
		if !c.present() {
			failed++
			fmt.Fprintf(out, "%s isn't installed here. To add Rustle by hand:\n  %s\n", c.label, strings.ReplaceAll(c.manual(r), "\n", "\n  "))
			continue
		}
		message, err := c.register(r)
		if err != nil {
			failed++
			fmt.Fprintf(out, "Could not add Rustle to %s: %v\nTo add it by hand:\n  %s\n", c.label, err, strings.ReplaceAll(c.manual(r), "\n", "\n  "))
			continue
		}
		fmt.Fprintln(out, message)
		done = append(done, c.label)
	}
	if len(done) > 0 {
		their := "its"
		if len(done) > 1 {
			their = "their"
		}
		fmt.Fprintf(out, "Restart %s, or reconnect %s MCP servers, to start using it.\n", list(done), their)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// list joins names as a sentence does: "A", "A and B", "A, B and C".
func list(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func find(id string) *client {
	for i := range clients {
		if clients[i].id == id {
			return &clients[i]
		}
	}
	return nil
}
