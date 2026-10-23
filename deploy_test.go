package main

import (
	"bytes"
	"testing"
)

func TestPinned(t *testing.T) {
	got := pinned(vercelDockerfile, "1.2.3")
	if !bytes.Contains(got, []byte("ARG VERSION=v1.2.3\n")) || !bytes.Contains(got, []byte("rustle-mcp@$VERSION;")) {
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
