package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

const name = "rustle"

var clients = []string{"claude", "codex", "cursor", "json"}

// binary is this executable, from where it will stay. Built by go run it sits in a
// temporary folder that disappears, so it is copied to a folder of its own first: the
// server is one self-contained file.
func binary() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}
	temp := rustle.Real(rustle.TempDir()) + string(filepath.Separator)
	if !strings.HasPrefix(self, temp) && !strings.Contains(self, string(filepath.Separator)+"go-build") {
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

func setup(argv []string) int {
	client := ""
	for _, a := range argv {
		if contains(clients, a) {
			client = a
			break
		}
	}
	printOnly := contains(argv, "--print")
	scope := "user"
	for i, a := range argv {
		if a == "--scope" && i+1 < len(argv) && argv[i+1] != "" {
			scope = argv[i+1]
		}
	}
	// Folders run until the next flag, so a later "--scope project" keeps its value.
	var roots []string
	for i, a := range argv {
		if a != "--media-roots" {
			continue
		}
		for _, r := range argv[i+1:] {
			if strings.HasPrefix(r, "--") {
				break
			}
			if !contains(clients, r) {
				if abs, err := filepath.Abs(r); err == nil {
					roots = append(roots, abs)
				}
			}
		}
		break
	}
	command, err := binary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rustle: %v\n", err)
		return 1
	}
	var envFlags []string
	type entry struct {
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env,omitempty"`
	}
	server := entry{Command: command, Args: []string{}}
	if len(roots) > 0 {
		value := strings.Join(roots, rustle.PathListSeparator)
		envFlags = []string{"--env", "RUSTLE_MEDIA_ROOTS=" + value}
		server.Env = map[string]string{"RUSTLE_MEDIA_ROOTS": value}
	}
	commands := map[string][]string{
		"claude": append(append([]string{"claude", "mcp", "add", "--scope", scope}, envFlags...), name, "--", command),
		"codex":  append(append([]string{"codex", "mcp", "add"}, envFlags...), name, "--", command),
	}
	if run, ok := commands[client]; ok && !printOnly {
		fmt.Println("Running: " + quote(run))
		cmd := exec.Command(run[0], run[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		err := cmd.Run()
		if _, missing := err.(*exec.Error); !missing {
			restart := fmt.Sprintf("Restart %s or reconnect its MCP servers.", client)
			if err == nil {
				fmt.Printf("Registered '%s' with %s. %s\n", name, client, restart)
				return 0
			}
			// The CLI refuses a name it already has. Running setup again is how an update
			// lands, and an entry that already runs this binary now runs the new one.
			if out, _ := exec.Command(client, "mcp", "get", name).Output(); strings.Contains(string(out), command) {
				fmt.Printf("'%s' already runs %s, which is now updated. %s\n", name, command, restart)
				return 0
			}
			remove := []string{client, "mcp", "remove"}
			if client == "claude" {
				remove = append(remove, "--scope", scope)
			}
			fmt.Printf("To replace the '%s' already registered, remove it and run setup again: %s\n", name, quote(append(remove, name)))
			if exit, ok := err.(*exec.ExitError); ok {
				return exit.ExitCode()
			}
			return 1
		}
		fmt.Printf("The %s CLI is not on PATH; run this yourself:\n\n", client)
	}
	asJSON, _ := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{name: server}}, "", "  ")
	compact, _ := json.Marshal(server)
	sections := map[string]string{
		"claude": "Claude Code:\n  " + quote(commands["claude"]),
		"codex":  "Codex CLI:\n  " + quote(commands["codex"]),
		"cursor": "Cursor (open this link, or paste the JSON into ~/.cursor/mcp.json):\n  cursor://anysphere.cursor-deeplink/mcp/install?name=" +
			name + "&config=" + base64.StdEncoding.EncodeToString(compact),
		"json": "Any client that reads a JSON server list:\n" + string(asJSON),
	}
	chosen := clients
	switch client {
	case "cursor":
		chosen = []string{"cursor", "json"}
	case "":
	default:
		chosen = []string{client}
	}
	parts := make([]string, len(chosen))
	for i, c := range chosen {
		parts[i] = sections[c]
	}
	fmt.Println(strings.Join(parts, "\n\n"))
	return 0
}

func contains(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}
