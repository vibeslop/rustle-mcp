// Rustle is a stdio MCP server that lets agents read videos: transcripts, frames and
// sound from YouTube, web pages, media URLs and local files.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"

	"github.com/vibeslop/rustle-mcp/internal/mcp"
	"github.com/vibeslop/rustle-mcp/internal/media"
	"github.com/vibeslop/rustle-mcp/internal/proc"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
	"github.com/vibeslop/rustle-mcp/internal/tools"
	"github.com/vibeslop/rustle-mcp/internal/youtube"
)

// version is set at release builds (-ldflags "-X main.version=1.2.3"); go install of a
// tag records the module version instead.
var version = ""

// release is a module version that names a tag, not a commit.
var release = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

func versionString() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && release.MatchString(info.Main.Version) {
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	return "dev"
}

const help = `rustle-mcp %s: MCP server that lets AI agents read videos.

  rustle-mcp                     serve MCP over stdio
  rustle-mcp setup [client]      register with claude or codex, or print config (cursor, json)
      --scope user|project|local   Claude Code scope (default user)
      --media-roots DIR ...        folders local files may come from (default: home, temp, cwd)
      --print                      print the command instead of running it
`

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "setup" {
		os.Exit(setup(args[1:]))
	}
	if slices.Contains(args, "--version") {
		fmt.Println(versionString())
		return
	}
	if slices.Contains(args, "--help") || slices.Contains(args, "-h") {
		fmt.Printf(help, versionString())
		return
	}
	settings, err := rustle.LoadSettings(rustle.ProcessEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rustle: %v\n", err)
		os.Exit(2)
	}
	_ = os.MkdirAll(settings.CacheDir, 0o700)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// Killed by the client: the link folder of waiting decoders goes with the server.
		proc.Cleanup()
		os.Exit(0)
	}()

	server := &mcp.Server{
		Name: "rustle", Version: versionString(), Instructions: tools.Instructions,
		Tools: tools.NewServer(settings, tools.Default).Tools(),
	}
	// Every YouTube call needs a visitor id, a round trip to youtube.com. Fetching one now
	// (when none is cached) takes that trip and the TLS handshake off the first call.
	// RUSTLE_PREFETCH=0 turns it off.
	if os.Getenv("RUSTLE_PREFETCH") != "0" {
		go func() {
			call := &rustle.Call{Ctx: ctx, Settings: settings, Cache: rustle.NewCache(settings.CacheDir, settings.CacheBytes), Notes: &rustle.Notes{}}
			_, _ = youtube.VisitorData(call, false)
		}()
	}
	// A client starts its servers before the agent's first call, so the server gets
	// ready for it meanwhile: a few decoders load ffmpeg's libraries and wait (see proc's
	// warm.go).
	go media.PrewarmDecoders(settings)

	if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "rustle: %v\n", err)
	}
	proc.Cleanup()
}
