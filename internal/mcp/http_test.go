package mcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func request(url, token, session, body string) (*http.Response, error) {
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if session != "" {
		req.Header.Set(sessionHeader, session)
	}
	return http.DefaultClient.Do(req)
}

func post(t *testing.T, url, token, session, body string) *http.Response {
	t.Helper()
	resp, err := request(url, token, session, body)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func read(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestHTTPNeedsTheToken(t *testing.T) {
	ts := httptest.NewServer((&Server{}).Handler("secret"))
	defer ts.Close()
	for _, token := range []string{"", "wrong"} {
		if resp := post(t, ts.URL+"/mcp", token, "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q: status %d", token, resp.StatusCode)
		}
	}
	if resp := post(t, ts.URL+"/mcp", "secret", "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); resp.StatusCode != http.StatusOK {
		t.Errorf("right token: status %d", resp.StatusCode)
	}
	// Clients that can't send a header carry the token in the address.
	if resp := post(t, ts.URL+"/mcp/secret", "", "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); resp.StatusCode != http.StatusOK {
		t.Errorf("token in the address: status %d", resp.StatusCode)
	}
	if resp := post(t, ts.URL+"/mcp/wrong", "", "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token in the address: status %d", resp.StatusCode)
	}
}

// The session id carries the client's name, so a call reaches its tool with it even on
// another instance of the server.
func TestHTTPSessionCarriesTheClient(t *testing.T) {
	var seen string
	tool := Tool{Name: "echo", Call: func(_ context.Context, client string, _ map[string]any) Result {
		seen = client
		return Result{}
	}}
	first := httptest.NewServer((&Server{Name: "rustle", Tools: []Tool{tool}}).Handler("t"))
	defer first.Close()
	resp := post(t, first.URL+"/mcp", "t", "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"claude-ai"}}}`)
	session := resp.Header.Get(sessionHeader)
	if body := read(t, resp); !strings.Contains(body, `"protocolVersion":"2025-06-18"`) || session == "" {
		t.Fatalf("initialize: session %q, body %s", session, body)
	}
	if resp := post(t, first.URL+"/mcp", "t", session, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); resp.StatusCode != http.StatusAccepted {
		t.Errorf("notification: status %d", resp.StatusCode)
	}
	second := httptest.NewServer((&Server{Name: "rustle", Tools: []Tool{tool}}).Handler("t"))
	defer second.Close()
	resp = post(t, second.URL+"/mcp", "t", session, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
	if body := read(t, resp); !strings.Contains(body, `"id":2`) || resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("tools/call: %s", body)
	}
	if seen != "claude-ai" {
		t.Errorf("client %q", seen)
	}
}

func TestHTTPCancelsWithinTheSession(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	tool := Tool{Name: "wait", Call: func(ctx context.Context, _ string, _ map[string]any) Result {
		close(started)
		select {
		case <-ctx.Done():
			close(cancelled)
		case <-time.After(5 * time.Second):
		}
		return Result{}
	}}
	ts := httptest.NewServer((&Server{Tools: []Tool{tool}}).Handler("t"))
	defer ts.Close()
	go request(ts.URL, "t", "a.b", `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"wait"}}`)
	<-started
	// Another session's request 7 is a different call.
	post(t, ts.URL, "t", "other", `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`)
	select {
	case <-cancelled:
		t.Fatal("cancelled by another session")
	case <-time.After(50 * time.Millisecond):
	}
	post(t, ts.URL, "t", "a.b", `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`)
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("not cancelled")
	}
}

func TestHTTPGet(t *testing.T) {
	ts := httptest.NewServer((&Server{Name: "rustle"}).Handler("t"))
	defer ts.Close()
	// A health check gets an answer without the token, and a person the steps to connect,
	// with this server's address in them.
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if body := read(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(body, "claude mcp add --transport http rustle "+ts.URL+"/mcp/YOUR_TOKEN") {
		t.Errorf("GET /: %d %s", resp.StatusCode, body)
	}
	page, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	page.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err = http.DefaultClient.Do(page)
	if err != nil {
		t.Fatal(err)
	}
	if body := read(t, resp); !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(body, "<code>codex mcp add rustle --url "+ts.URL+"/mcp/YOUR_TOKEN") {
		t.Errorf("browser: %s %s", resp.Header.Get("Content-Type"), body)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/mcp", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("event stream: status %d", resp.StatusCode)
	}
}
