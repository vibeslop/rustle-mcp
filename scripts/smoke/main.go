// Smoke checks the built server against YouTube, which changes without notice: every tool
// once, through stdio as a client calls it, with a fresh cache. Needs the network and ffmpeg.
//
//	go run ./scripts/smoke
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// video is "Me at the zoo": the first YouTube video, 19 s, with captions, unchanged since 2005.
const video = "jNQXAC9IVRw"

type check struct {
	tool   string
	args   map[string]any
	passes func(text string, images int) bool
}

var checks = []check{
	{"read_transcript", map[string]any{"source": video}, func(text string, _ int) bool {
		return regexp.MustCompile(`(?i)elephants`).MatchString(text)
	}},
	{"watch_video", map[string]any{"source": video, "frames": 4}, func(_ string, images int) bool { return images == 1 }},
	{"inspect_frame", map[string]any{"source": video, "at_seconds": 5, "crop": []float64{0.25, 0.25, 0.5, 0.5}}, func(text string, images int) bool {
		return images == 1 && strings.Contains(text, `"at"`)
	}},
	{"listen_video", map[string]any{"source": video, "seconds": 5, "output": "analysis"}, func(text string, images int) bool {
		return images == 1 && strings.Contains(text, "rms_dbfs")
	}},
	{"find_videos", map[string]any{"query": "me at the zoo", "limit": 5}, func(text string, _ int) bool { return strings.Contains(text, video) }},
}

func main() {
	work, err := os.MkdirTemp("", "rustle-smoke-")
	if err != nil {
		fail(err)
	}
	defer os.RemoveAll(work)
	binary := filepath.Join(work, "rustle-mcp")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fail(err)
	}
	server := exec.Command(binary)
	server.Env = append(os.Environ(), "RUSTLE_CACHE="+filepath.Join(work, "cache"))
	server.Stderr = os.Stderr
	stdin, _ := server.StdinPipe()
	stdout, _ := server.StdoutPipe()
	if err := server.Start(); err != nil {
		fail(err)
	}
	defer server.Process.Kill()
	replies := bufio.NewScanner(stdout)
	replies.Buffer(make([]byte, 1<<20), 64<<20)
	id := 0
	request := func(method string, params any) map[string]any {
		id++
		line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		fmt.Fprintf(stdin, "%s\n", line)
		for replies.Scan() {
			var message map[string]any
			if json.Unmarshal(replies.Bytes(), &message) == nil && message["id"] == float64(id) {
				return message
			}
		}
		fail(fmt.Errorf("the server closed its output"))
		return nil
	}
	request("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "claude-code", "version": "0"}})
	fmt.Fprintln(stdin, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	failed := 0
	for _, c := range checks {
		began := time.Now()
		reply := request("tools/call", map[string]any{"name": c.tool, "arguments": c.args})
		var texts []string
		images := 0
		result, _ := reply["result"].(map[string]any)
		content, _ := result["content"].([]any)
		for _, b := range content {
			block, _ := b.(map[string]any)
			switch block["type"] {
			case "text":
				texts = append(texts, fmt.Sprint(block["text"]))
			case "image":
				images++
			}
		}
		text := strings.Join(texts, "\n")
		ok := reply["error"] == nil && result["isError"] == nil && c.passes(text, images)
		if !ok {
			failed++
			if e, isError := reply["error"].(map[string]any); isError {
				text = fmt.Sprint(e["message"])
			}
		}
		status := "ok  "
		if !ok {
			status = "FAIL"
		}
		first, _, _ := strings.Cut(text, "\n")
		fmt.Printf("%s %-16s %5d ms  %.100s\n", status, c.tool, time.Since(began).Milliseconds(), first)
	}
	if failed > 0 {
		os.Exit(1)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "smoke: %v\n", err)
	os.Exit(1)
}
