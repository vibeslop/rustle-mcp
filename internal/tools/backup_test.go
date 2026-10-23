package tools

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vibeslop/rustle-mcp/internal/mcp"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// A call YouTube refuses goes to the backup, which answers it for the same client, marked
// so that it won't pass the call on.
func TestRefusedCallsGoToTheBackup(t *testing.T) {
	var client string
	var forwarded bool
	backup := httptest.NewServer((&mcp.Server{Tools: []mcp.Tool{{Name: "watch_video", Call: func(ctx context.Context, name string, args map[string]any) mcp.Result {
		client, forwarded = name, mcp.Forwarded(ctx)
		if args["source"] == "captionless" {
			return mcp.Result{IsError: true, Content: []rustle.Block{rustle.TextBlock(`{"error":"No captions for this video.","code":"NO_CAPTIONS"}`)}}
		}
		return mcp.Result{Content: []rustle.Block{rustle.TextBlock("from the backup: " + args["source"].(string))}}
	}}}}).Handler("t"))
	defer backup.Close()

	settings := testSettings(t).Copy()
	settings.Backup = backup.URL + "/mcp/t"
	code := "YOUTUBE_ACCESS_DENIED"
	refused := func(*rustle.Call, WatchArgs) (*rustle.Result, error) {
		return nil, rustle.Fail("YouTube refused.", code, "")
	}
	transcript := func() mcp.Tool {
		for _, tool := range NewServer(settings, Ops{Watch: refused}).Tools() {
			if tool.Name == "watch_video" {
				return tool
			}
		}
		return mcp.Tool{}
	}
	// A 429 is a refusal too: YouTube rate limits one region's WARP exit while another's answers.
	for _, code = range []string{"YOUTUBE_ACCESS_DENIED", "RATE_LIMITED"} {
		client, forwarded = "", false
		result := transcript().Call(context.Background(), "claude-ai", map[string]any{"source": "jNQXAC9IVRw"})
		if result.IsError || len(result.Content) != 1 || result.Content[0].Text != "from the backup: jNQXAC9IVRw" {
			t.Fatalf("%s: result %+v", code, result)
		}
		if client != "claude-ai" || !forwarded {
			t.Errorf("%s: backup saw client %q, forwarded %v", code, client, forwarded)
		}
	}
	code = "YOUTUBE_ACCESS_DENIED"

	// The backup's own errors are its answer.
	result := transcript().Call(context.Background(), "claude-ai", map[string]any{"source": "captionless"})
	if !result.IsError || !strings.Contains(result.Content[0].Text, "NO_CAPTIONS") {
		t.Errorf("backup's error: %+v", result)
	}

	settings.Backup = ""
	result = transcript().Call(context.Background(), "claude-ai", map[string]any{"source": "x"})
	if !result.IsError || !strings.Contains(result.Content[0].Text, "YOUTUBE_ACCESS_DENIED") {
		t.Errorf("without a backup: %+v", result)
	}
}

// A server that received a forwarded call makes it itself, even with a backup of its own,
// so two servers that back each other up never pass a call back and forth.
func TestForwardedCallsStay(t *testing.T) {
	asked := 0
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { asked++ }))
	defer backup.Close()
	settings := testSettings(t).Copy()
	settings.Backup = backup.URL
	refused := func(*rustle.Call, WatchArgs) (*rustle.Result, error) {
		return nil, rustle.Fail("YouTube refused.", "YOUTUBE_ACCESS_DENIED", "")
	}
	server := httptest.NewServer((&mcp.Server{Tools: NewServer(settings, Ops{Watch: refused}).Tools()}).Handler("t"))
	defer server.Close()
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/mcp/t", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"watch_video","arguments":{"source":"x"}}}`))
	req.Header.Set(mcp.ForwardedHeader, "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if asked != 0 || !strings.Contains(string(body), "YOUTUBE_ACCESS_DENIED") {
		t.Errorf("backup asked %d times; reply %s", asked, body)
	}
}
