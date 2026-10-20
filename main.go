// Rustle is an MCP server that lets agents read videos: transcripts, frames and sound
// from YouTube, web pages, media URLs and local files. It runs over stdio next to the
// agent, or over HTTP when hosted.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/fetch"
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
  rustle-mcp http                serve MCP over HTTP on $PORT (default 8080), for hosting;
                                 requests carry RUSTLE_TOKEN, as /mcp/<token> or a bearer token
  rustle-mcp warp                register a free Cloudflare WARP device and print its
                                 WireGuard profile, which a hosted server tunnels through
  rustle-mcp setup [client...]   add Rustle to the clients named (claude, codex, cursor), or
                                 to those installed: ticked in a list at a terminal, all of
                                 them in a script; json prints the config for any other client
      --scope user|project|local   Claude Code scope (default user)
      --media-roots DIR ...        folders local files may come from (default: home, temp, cwd)
      --print                      print what to run or paste instead of doing it
`

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "setup" {
		os.Exit(setup(args[1:]))
	}
	if len(args) > 0 && args[0] == "warp" {
		os.Exit(warp())
	}
	if slices.Contains(args, "--version") {
		fmt.Println(versionString())
		return
	}
	if slices.Contains(args, "--help") || slices.Contains(args, "-h") {
		fmt.Printf(help, versionString())
		return
	}
	hosted := len(args) > 0 && args[0] == "http"
	env := rustle.ProcessEnv
	token := os.Getenv("RUSTLE_TOKEN")
	if hosted {
		// Anyone who finds a hosted server's address could otherwise spend its bandwidth
		// and, through a proxy, the owner's money.
		if token == "" {
			fmt.Fprintln(os.Stderr, "rustle: set RUSTLE_TOKEN to a long random string (openssl rand -hex 32); clients connect to /mcp/<token>.")
			os.Exit(2)
		}
		// A hosted server's files are not its users': local paths are off unless
		// RUSTLE_MEDIA_ROOTS names folders.
		env = func(name string) (string, bool) {
			if value, ok := os.LookupEnv(name); ok || name != "RUSTLE_MEDIA_ROOTS" {
				return value, ok
			}
			return "", true
		}
	}
	settings, err := rustle.LoadSettings(env)
	var wireproxy *exec.Cmd
	if err == nil {
		// Proxies to try in order, moving down the list while YouTube refuses one.
		proxies := strings.Split(os.Getenv("RUSTLE_PROXIES"), ",")
		if hosted && os.Getenv("RUSTLE_PROXIES") == "" {
			proxies, wireproxy = tunnel()
		}
		err = fetch.SetRoutes(proxies)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "rustle: %v\n", err)
		os.Exit(2)
	}
	_ = os.MkdirAll(settings.CacheDir, 0o700)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if !hosted {
		go func() {
			<-ctx.Done()
			// Killed by the client: the link folder of waiting decoders goes with the server.
			proc.Cleanup()
			os.Exit(0)
		}()
	}

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

	if hosted {
		err = serveHTTP(ctx, server, token)
	} else {
		err = server.Serve(ctx, os.Stdin, os.Stdout)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "rustle: %v\n", err)
	}
	if wireproxy != nil {
		_ = wireproxy.Process.Kill()
	}
	proc.Cleanup()
}

// serveHTTP serves MCP on $PORT, the port hosting platforms assign, until ctx ends; calls
// still running then get a few seconds to finish.
func serveHTTP(ctx context.Context, server *mcp.Server, token string) error {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	listener := &http.Server{Addr: ":" + port, Handler: server.Handler(token), ReadHeaderTimeout: 10 * time.Second}
	closed := make(chan struct{})
	go func() {
		<-ctx.Done()
		grace, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = listener.Shutdown(grace)
		close(closed)
	}()
	fmt.Fprintf(os.Stderr, "rustle: serving MCP over HTTP on port %s\n", port)
	if err := listener.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	<-closed
	return nil
}
