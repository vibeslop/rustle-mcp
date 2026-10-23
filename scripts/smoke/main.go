// Smoke checks the built server against YouTube, which changes without notice: every tool
// once, through stdio as a client calls it, with a fresh cache. Needs the network and ffmpeg.
//
//	go run ./scripts/smoke
//
// Given a server's address, it checks a hosted server instead, which shows whether YouTube
// answers the host's address:
//
//	go run ./scripts/smoke https://rustle.example.com/mcp/<token>
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	{"watch_video", map[string]any{"source": video, "frames": 0}, func(text string, images int) bool {
		return images == 0 && regexp.MustCompile(`(?i)elephants`).MatchString(text)
	}},
	// By default, a frame a second from YouTube's smaller format: two sheets for 19 s, then
	// every word under the frames' labels.
	{"watch_video", map[string]any{"source": video}, func(text string, images int) bool {
		return images == 2 && regexp.MustCompile(`(?m)^#\d+ 0:\d\d\.\d\d$`).MatchString(text) && regexp.MustCompile(`(?i)elephants`).MatchString(text)
	}},
	// An overview: frames spread across the video.
	{"watch_video", map[string]any{"source": video, "frames": 4}, func(_ string, images int) bool { return images == 1 }},
	{"inspect_frame", map[string]any{"source": video, "at_seconds": 5, "crop": []float64{0.25, 0.25, 0.5, 0.5}}, func(text string, images int) bool {
		return images == 1 && strings.Contains(text, `"at"`)
	}},
	{"watch_video", map[string]any{"source": video, "end_seconds": 5, "frames": 0, "listen": "analysis"}, func(text string, images int) bool {
		return images == 1 && strings.Contains(text, "rms_dbfs")
	}},
	// Results depend on the address asking; through a VPN the video can rank below fifth.
	{"find_videos", map[string]any{"query": "me at the zoo jawed", "limit": 10}, func(text string, _ int) bool { return strings.Contains(text, video) }},
}

func main() {
	var call func(message map[string]any) map[string]any
	if len(os.Args) > 1 {
		call = hosted(os.Args[1], os.Getenv("RUSTLE_TOKEN"))
	} else {
		var stop func()
		call, stop = local()
		defer stop()
	}
	id := 0
	request := func(method string, params any) map[string]any {
		id++
		return call(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	}
	request("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "claude-code", "version": "0"}})
	call(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
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

// local builds the server and talks to it over stdio, with a fresh cache.
func local() (call func(map[string]any) map[string]any, stop func()) {
	work, err := os.MkdirTemp("", "rustle-smoke-")
	if err != nil {
		fail(err)
	}
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
	replies := bufio.NewScanner(stdout)
	replies.Buffer(make([]byte, 1<<20), 64<<20)
	call = func(message map[string]any) map[string]any {
		line, _ := json.Marshal(message)
		fmt.Fprintf(stdin, "%s\n", line)
		if message["id"] == nil {
			return nil
		}
		for replies.Scan() {
			var reply map[string]any
			if json.Unmarshal(replies.Bytes(), &reply) == nil && reply["id"] == float64(message["id"].(int)) {
				return reply
			}
		}
		fail(fmt.Errorf("the server closed its output"))
		return nil
	}
	return call, func() {
		server.Process.Kill()
		os.RemoveAll(work)
	}
}

// hosted talks to a server over HTTP, carrying the session id it hands out.
func hosted(url, token string) func(map[string]any) map[string]any {
	session := ""
	return func(message map[string]any) map[string]any {
		body, _ := json.Marshal(message)
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			fail(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fail(err)
		}
		defer resp.Body.Close()
		if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
			session = id
		}
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			fail(fmt.Errorf("%v: HTTP %d: %s", message["method"], resp.StatusCode, strings.TrimSpace(string(data))))
		}
		var reply map[string]any
		if message["id"] != nil && json.Unmarshal(data, &reply) != nil {
			fail(fmt.Errorf("%v: not JSON: %.200s", message["method"], data))
		}
		return reply
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "smoke: %v\n", err)
	os.Exit(1)
}
