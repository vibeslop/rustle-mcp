package mcp

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestServeSkipsAByteOrderMark(t *testing.T) {
	var out bytes.Buffer
	in := strings.NewReader("\ufeff{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}\r\n")
	if err := (&Server{OnError: func(err error) { t.Error(err) }}).Serve(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, `"id":1`) {
		t.Errorf("reply %q", got)
	}
}
