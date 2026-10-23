package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

func TestPinned(t *testing.T) {
	got := pinned(vercelDockerfile, "1.2.3")
	// No line ending in the checks: Windows checkouts end the embedded file's lines in CRLF.
	if bytes.Contains(got, []byte("ARG VERSION=latest")) || !bytes.Contains(got, []byte("ARG VERSION=v1.2.3")) || !bytes.Contains(got, []byte("rustle-mcp@$VERSION;")) {
		t.Errorf("release not pinned:\n%s", got)
	}
	for _, version := range []string{"dev", "1.2.3-0.20261020214600-4ad0bf9c1a2b"} {
		if got := pinned(vercelDockerfile, version); !bytes.Equal(got, vercelDockerfile) {
			t.Errorf("%s pinned:\n%s", version, got)
		}
	}
}

func TestProductionURL(t *testing.T) {
	answer := []byte(`Vercel CLI 64.7.0
{"status": "ok", "deployment": {"id": "dpl_1", "url": "https://rustle-mcp-abc123-team.vercel.app", "productionUrl": "https://rustle-mcp-example.vercel.app"}}`)
	if got, err := productionURL(answer); err != nil || got != "https://rustle-mcp-example.vercel.app" {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := productionURL([]byte(`{"status": "error"}`)); err == nil {
		t.Error("no address accepted")
	}
}

// A refused first server is covered by the backup, so deploy must not read as broken then.
func TestVerdict(t *testing.T) {
	refused := rustle.Fail("YouTube is rate limiting this connection (HTTP 429).", "RATE_LIMITED", "")
	broken := errors.New("HTTP 502: Bad Gateway")
	for _, c := range []struct {
		name, region  string
		first, second error
		want          string
	}{
		{"rustle-mcp", "fra1", nil, nil, ""},
		{"rustle-mcp", "fra1", refused, nil, "The backup makes the YouTube calls Frankfurt can't, so the address works. Nothing to do."},
		{"rustle-mcp", "fra1", nil, refused, "The address works, but until the backup recovers"},
		{"rustle-mcp", "fra1", refused, refused, "move to San Francisco now: rustle-mcp deploy --region sfo1"},
		{"mine", "sfo1", refused, broken, "rustle-mcp deploy --region gru1 --name mine"},
		{"mine", "fra1", broken, nil, "Clients can't use the address until Frankfurt answers; its logs are on vercel.com, in the project mine."},
	} {
		got := verdict(c.name, c.region, c.first, c.second)
		if c.want == "" && got != "" || !strings.Contains(got, c.want) {
			t.Errorf("%v, %v: %q", c.first, c.second, got)
		}
	}
	if got := checkLine("Frankfurt", broken); got != "  Frankfurt: not answering (HTTP 502: Bad Gateway)." {
		t.Errorf("down: %q", got)
	}
}

// The check keeps the code of the server's error, which is how a refusal differs from a
// server that is down.
func TestCheckHostedKeepsTheCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"{\"error\":\"YouTube is rate limiting this connection (HTTP 429).\",\"code\":\"RATE_LIMITED\"}"}],"isError":true}}`)
	}))
	defer server.Close()
	err := checkHosted(server.URL)
	if err == nil || down(err) || err.Error() != "YouTube is rate limiting this connection (HTTP 429)." {
		t.Errorf("got %v", err)
	}
}
