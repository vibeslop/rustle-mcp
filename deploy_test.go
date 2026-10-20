package main

import "testing"

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
