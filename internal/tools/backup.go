package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/mcp"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// backupClient goes straight to the backup: its address needs no tunnel, and a paid proxy
// shouldn't carry it.
var backupClient = &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{Proxy: nil, ForceAttemptHTTP2: true}}

// forward asks the backup, another Rustle server in another region, to make a call YouTube
// refused here: YouTube trusts some regions' addresses and not others, and a region's
// trust can drop at any time. The backup keeps no state, so a session id naming the client
// is all the call needs.
func forward(ctx context.Context, backup, client, tool string, args map[string]any) (mcp.Result, error) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, backup, bytes.NewReader(body))
	if err != nil {
		return mcp.Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", mcp.NewSession(client))
	req.Header.Set(mcp.ForwardedHeader, "1")
	resp, err := backupClient.Do(req)
	if err != nil {
		return mcp.Result{}, err
	}
	defer resp.Body.Close()
	var reply struct {
		Result *struct {
			Content []struct {
				Type     string         `json:"type"`
				Text     string         `json:"text"`
				Data     string         `json:"data"`
				MimeType string         `json:"mimeType"`
				Meta     map[string]any `json:"_meta"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil || reply.Result == nil {
		return mcp.Result{}, fmt.Errorf("the backup answered HTTP %d without a result", resp.StatusCode)
	}
	// The backup's errors are answers too: a video without captions has none in any region.
	out := mcp.Result{IsError: reply.Result.IsError}
	for _, b := range reply.Result.Content {
		out.Content = append(out.Content, rustle.Block{Type: b.Type, Text: b.Text, Data: b.Data, MimeType: b.MimeType, Meta: b.Meta})
	}
	fmt.Fprintln(os.Stderr, "rustle: YouTube refused this server, and the backup answered")
	return out, nil
}
