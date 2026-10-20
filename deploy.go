package main

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/mcp"
)

// The files a Vercel deployment needs, so deploying works without a checkout.
var (
	//go:embed Dockerfile.vercel
	vercelDockerfile []byte
	//go:embed vercel.json
	vercelConfig []byte
)

const deployHelp = `rustle-mcp deploy: put Rustle on Vercel, for agents that can't start a local program.

Logs in to Vercel if needed, deploys a server and a backup in another region with one
private address, checks that YouTube answers them, and prints the address to give your
clients. Running it again updates both and keeps the address.

  --name NAME      Vercel project name (default rustle-mcp)
  --region REGION  Vercel region (default fra1, Frankfurt); a backup goes to another
`

// deploy puts Rustle on Vercel, the host it can set up alone: Vercel's CLI logs in and
// builds the container, and Rustle does the rest.
func deploy(argv []string) int {
	name, region := "rustle-mcp", ""
	for i := 0; i < len(argv); i++ {
		switch {
		case argv[i] == "--name" && i+1 < len(argv):
			i++
			name = argv[i]
		case argv[i] == "--region" && i+1 < len(argv):
			i++
			region = argv[i]
		case argv[i] == "--help" || argv[i] == "-h":
			fmt.Print(deployHelp)
			return 0
		default:
			fmt.Print(deployHelp)
			return 2
		}
	}
	if err := deployTo(name, region, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "rustle: %v\n", err)
		return 1
	}
	return 0
}

func deployTo(name, region string, out io.Writer) error {
	vercel, err := vercelCLI()
	if err != nil {
		return err
	}
	run := func(dir string, stdin []byte, args ...string) ([]byte, error) {
		cmd := exec.Command(vercel[0], append(vercel[1:], args...)...)
		cmd.Dir, cmd.Stdin = dir, bytes.NewReader(stdin)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		data, err := cmd.Output()
		if err != nil {
			return data, errors.New(lastLine(append(stderr.Bytes(), data...)))
		}
		return data, nil
	}
	if _, err := run("", nil, "whoami"); err != nil {
		fmt.Fprintln(out, "Log in to Vercel to continue.")
		login := exec.Command(vercel[0], append(vercel[1:], "login")...)
		login.Stdin, login.Stdout, login.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := login.Run(); err != nil {
			return fmt.Errorf("logging in to Vercel failed: %v", err)
		}
	}

	// From a checkout, its own code is deployed; anywhere else, the latest release.
	dir, err := os.Getwd()
	if err != nil || !checkout(dir) {
		if dir, err = os.MkdirTemp("", "rustle-deploy-"); err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile.vercel"), vercelDockerfile, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "vercel.json"), vercelConfig, 0o644); err != nil {
			return err
		}
	}

	// One server for each of two regions: YouTube trusts some regions' addresses and not
	// others, and a region's trust can drop at any time, so the main server hands the calls
	// YouTube refuses it to the backup. Both carry the same token, kept on this machine so
	// deploying again keeps the address clients use.
	if region == "" {
		region = "fra1"
	}
	spare := "iad1"
	if region == spare {
		spare = "fra1"
	}
	tokenFile, token, fresh := deployToken(name)
	one := func(project, region string, env map[string]string) (string, error) {
		if _, err := run(dir, nil, "link", "--yes", "--project", project); err != nil {
			return "", fmt.Errorf("creating the Vercel project %s failed: %v", project, err)
		}
		for key, value := range env {
			if _, err := run(dir, []byte(value), "env", "add", key, "production", "--sensitive", "--force"); err != nil {
				return "", fmt.Errorf("setting %s on Vercel failed: %v", key, err)
			}
		}
		data, err := run(dir, nil, "deploy", "--prod", "--yes", "--json", "--regions", region)
		if err != nil {
			return "", fmt.Errorf("deploying %s failed: %v", project, err)
		}
		base, err := productionURL(data)
		return base + "/mcp/" + token, err
	}
	fmt.Fprintf(out, "Deploying two servers to Vercel, in %s and a backup in %s; building them takes a few minutes.\n", place(region), place(spare))
	backup, err := one(name+"-backup", spare, map[string]string{"RUSTLE_TOKEN": token})
	if err != nil {
		return err
	}
	address, err := one(name, region, map[string]string{"RUSTLE_TOKEN": token, "RUSTLE_BACKUP": backup})
	if err != nil {
		return err
	}

	fmt.Fprintln(out, "Checking that YouTube answers each.")
	for _, server := range []struct{ label, address string }{{place(region), address}, {"the backup in " + place(spare), backup}} {
		if err := checkHosted(server.address); err != nil {
			fmt.Fprintf(out, "  %s: %v\n", server.label, err)
		} else {
			fmt.Fprintf(out, "  YouTube answers %s.\n", server.label)
		}
	}

	note := ""
	if fresh {
		note = " It is new: give it to clients again if they used an older one."
	}
	fmt.Fprintf(out, `
Rustle is running. Its private address works like a password:

  %s

claude.ai: Settings, Connectors, Add custom connector; paste the address, with No sign-in.
Claude Code: claude mcp add --transport http rustle %s
Codex: codex mcp add rustle --url %s
Cursor, in mcp.json: "rustle": {"url": "%s"}

The address is saved in %s.%s
`, address, address, address, address, tokenFile, note)
	return nil
}

// place names a Vercel region by its city.
func place(region string) string {
	cities := map[string]string{"fra1": "Frankfurt", "iad1": "Washington", "sfo1": "San Francisco", "pdx1": "Portland", "cle1": "Cleveland",
		"gru1": "São Paulo", "lhr1": "London", "cdg1": "Paris", "dub1": "Dublin", "arn1": "Stockholm", "hnd1": "Tokyo", "kix1": "Osaka",
		"icn1": "Seoul", "sin1": "Singapore", "hkg1": "Hong Kong", "bom1": "Mumbai", "syd1": "Sydney", "cpt1": "Cape Town", "yul1": "Montreal"}
	if city, ok := cities[region]; ok {
		return city
	}
	return region
}

// vercelCLI is the Vercel CLI, or npx running it, which most Node installs have.
func vercelCLI() ([]string, error) {
	if path := cli("vercel"); path != "" {
		return []string{path}, nil
	}
	if path := cli("npx"); path != "" {
		return []string{path, "--yes", "vercel@latest"}, nil
	}
	return nil, errors.New("deploying uses the Vercel CLI, which needs Node.js: install it from https://nodejs.org, then run this again")
}

// checkout is true in a clone of Rustle, whose own code then gets deployed.
func checkout(dir string) bool {
	mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	_, statErr := os.Stat(filepath.Join(dir, "Dockerfile.vercel"))
	return err == nil && statErr == nil && bytes.HasPrefix(mod, []byte("module github.com/vibeslop/rustle-mcp\n"))
}

// deployToken is the project's token from this machine, or a new one, saved.
func deployToken(name string) (file, token string, fresh bool) {
	config, err := os.UserConfigDir()
	if err != nil {
		config = os.TempDir()
	}
	file = filepath.Join(config, "rustle-mcp", "vercel-"+name+".token")
	if data, err := os.ReadFile(file); err == nil && len(bytes.TrimSpace(data)) >= 32 {
		return file, string(bytes.TrimSpace(data)), false
	}
	random := make([]byte, 24)
	_, _ = rand.Read(random)
	token = hex.EncodeToString(random)
	_ = os.MkdirAll(filepath.Dir(file), 0o700)
	_ = os.WriteFile(file, []byte(token+"\n"), 0o600)
	return file, token, true
}

// productionURL reads the project's own address from the CLI's JSON, rather than the one
// each deployment gets.
func productionURL(data []byte) (string, error) {
	var answer struct {
		Deployment struct {
			URL           string `json:"url"`
			ProductionURL string `json:"productionUrl"`
		} `json:"deployment"`
	}
	_ = json.Unmarshal(data[max(0, bytes.IndexByte(data, '{')):], &answer)
	address := answer.Deployment.ProductionURL
	if address == "" {
		address = answer.Deployment.URL
	}
	if !strings.HasPrefix(address, "https://") {
		return "", fmt.Errorf("the Vercel CLI's answer has no deployment address: %.200s", data)
	}
	return strings.TrimSuffix(address, "/"), nil
}

// checkHosted asks a new server for a transcript, which needs YouTube to answer it: the
// server itself, never its backup. The first request also waits out a cold start.
func checkHosted(address string) error {
	client := &http.Client{Timeout: 90 * time.Second}
	session := ""
	call := func(body string) (string, error) {
		req, _ := http.NewRequest(http.MethodPost, address, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set(mcp.ForwardedHeader, "1")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
		}
		if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
			session = id
		}
		return string(data), nil
	}
	if _, err := call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"rustle-deploy"}}}`); err != nil {
		return err
	}
	// "Me at the zoo": the first YouTube video, unchanged since 2005.
	reply, err := call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_transcript","arguments":{"source":"jNQXAC9IVRw"}}}`)
	if err != nil {
		return err
	}
	if strings.Contains(reply, `"isError":true`) {
		var message struct {
			Result struct {
				Content []struct{ Text string } `json:"content"`
			} `json:"result"`
		}
		_ = json.Unmarshal([]byte(reply), &message)
		var failure struct {
			Error string `json:"error"`
		}
		if len(message.Result.Content) > 0 && json.Unmarshal([]byte(message.Result.Content[0].Text), &failure) == nil && failure.Error != "" {
			return errors.New(failure.Error)
		}
		return errors.New(reply)
	}
	return nil
}
