package mcp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"path"
	"strings"
)

// sessionHeader carries the session id the server hands out at initialize; clients send
// it back on every request.
const sessionHeader = "Mcp-Session-Id"

// ForwardedHeader marks a call one Rustle server passed to its backup, which then makes
// it itself rather than passing it on.
const ForwardedHeader = "X-Rustle-Forwarded"

type forwardedKey struct{}

// Forwarded is true in a call another Rustle server passed on.
func Forwarded(ctx context.Context) bool { return ctx.Value(forwardedKey{}) != nil }

// Handler serves MCP over Streamable HTTP at any path. Each POST carries one message and
// gets its reply as JSON. Rustle never sends requests of its own, so there is no event
// stream to open, and a session holds nothing but the client's name, which the session id
// carries: any instance of the server, or one restarted since, can answer any request.
// Every request must carry the token: as the address's last segment (/mcp/<token>), which
// every client can be given in one line, or as "Authorization: Bearer <token>".
func (s *Server) Handler(token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodDelete:
		case http.MethodGet:
			// A client asking for an event stream is told there is none; anyone else, a
			// browser or a platform's health check, is told what this is.
			if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
				w.Header().Set("Allow", "POST, DELETE")
				http.Error(w, "This server sends no messages of its own.", http.StatusMethodNotAllowed)
				return
			}
			s.setup(w, r)
			return
		default:
			w.Header().Set("Allow", "GET, POST, DELETE")
			http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
			return
		}
		if !authorized(r, token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "Missing or wrong token: connect to /mcp/<RUSTLE_TOKEN>.", http.StatusUnauthorized)
			return
		}
		// Ending a session frees nothing: the server keeps no state for it.
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "The request body is too large.", http.StatusRequestEntityTooLarge)
			return
		}
		if err != nil {
			return
		}
		m, ok := s.parse(body)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write(errorReply(nil, -32700, "Parse error: the body must be one JSON-RPC message"))
			return
		}
		session := r.Header.Get(sessionHeader)
		if m.notification() {
			s.notified(session, m)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var line []byte
		switch m.Method {
		case "initialize":
			result, client := s.initialize(m.Params)
			w.Header().Set(sessionHeader, NewSession(client))
			line = reply(m.ID, result)
		case "tools/call":
			// The call also ends when the client hangs up.
			ctx := r.Context()
			if r.Header.Get(ForwardedHeader) != "" {
				ctx = context.WithValue(ctx, forwardedKey{}, true)
			}
			call, done := s.track(ctx, session, m.ID)
			defer done()
			line = s.call(call, sessionClient(session), m)
			if call.Err() != nil {
				return
			}
		default:
			line = s.answer(m)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(line)
	})
}

func authorized(r *http.Request, token string) bool {
	same := func(given string) bool { return subtle.ConstantTimeCompare([]byte(given), []byte(token)) == 1 }
	scheme, bearer, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	return token != "" && (same(path.Base(r.URL.Path)) || strings.EqualFold(scheme, "Bearer") && same(strings.TrimSpace(bearer)))
}

// NewSession is a session id for the named client: random, then the name. The name only
// picks how images and audio are sent, so a client that forges it harms only itself.
func NewSession(client string) string {
	var id [12]byte
	_, _ = rand.Read(id[:])
	if len(client) > 64 {
		client = client[:64]
	}
	return hex.EncodeToString(id[:]) + "." + base64.RawURLEncoding.EncodeToString([]byte(client))
}

// sessionClient is the client name a session id carries; "" for an id without one.
func sessionClient(session string) string {
	_, name, _ := strings.Cut(session, ".")
	decoded, err := base64.RawURLEncoding.DecodeString(name)
	if err != nil {
		return ""
	}
	return string(decoded)
}

// setup is the page a browser gets: how to connect each client to this server, with its
// address filled in, so the step after deploying is copying a line.
func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	scheme := "https"
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme = forwarded
	} else if r.TLS == nil {
		scheme = "http"
	}
	url := scheme + "://" + r.Host + "/mcp/YOUR_TOKEN"
	steps := [][2]string{
		{"claude.ai: Settings, Connectors, Add custom connector, with No sign-in", url},
		{"Claude Code", "claude mcp add --transport http rustle " + url},
		{"Codex", "codex mcp add rustle --url " + url},
		{"Cursor, in mcp.json", `"rustle": {"url": "` + url + `"}`},
	}
	how := "Connect a client to its private address, " + url + ", with YOUR_TOKEN replaced by the server's RUSTLE_TOKEN; rustle-mcp deploy prints the whole address."
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		fmt.Fprintf(w, "%s is running. %s\n", strings.TrimSpace(s.Name+" "+s.Version), how)
		for _, step := range steps {
			fmt.Fprintf(w, "\n%s:\n  %s\n", step[0], step[1])
		}
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Rustle</title>
<style>body{font:15px/1.5 system-ui,sans-serif;max-width:44rem;margin:3rem auto;padding:0 1.25rem;color:#1d1d1f;background:#fbfbfa}
h1{font-size:1.4rem;margin:0 0 .5rem}p{color:#555;margin:0 0 1.5rem}h2{font-size:.9rem;font-weight:600;margin:1.5rem 0 .4rem}
code{display:block;padding:.75rem 1rem;border-radius:8px;background:#efefec;font:13px/1.5 ui-monospace,monospace;overflow-wrap:anywhere;user-select:all}
@media (prefers-color-scheme:dark){body{color:#ececec;background:#161616}p{color:#a0a0a0}code{background:#262626}}</style>
<h1>Rustle is running</h1><p>%s</p>`, html.EscapeString(how))
	for _, step := range steps {
		fmt.Fprintf(w, "<h2>%s</h2><code>%s</code>", html.EscapeString(step[0]), html.EscapeString(step[1]))
	}
}
