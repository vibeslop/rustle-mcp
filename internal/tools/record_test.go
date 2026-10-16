package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
	"github.com/vibeslop/rustle-mcp/internal/source"
)

func TestACacheThatCannotBeWrittenLosesTheDiskCopyOfARecordNotTheServer(t *testing.T) {
	// A file where the cache folder should be fails every write, even for root.
	root := filepath.Join(tempDir(t), "cache")
	write(t, root, "")
	cache := rustle.NewCache(root, 1<<20)
	record := &rustle.Record{Key: "clip", Kind: "local", Source: "/clip.mp4", Title: "clip.mp4", ResolvedAt: time.Now().UnixMilli(),
		Input: "/clip.mp4", Captions: []rustle.Caption{}}
	rustle.SaveRecord(record, cache)
	// The write runs after the call; failing there must not take the process down.
	rustle.FlushRecords()
	s := testSettings(t)
	again, err := source.Resolve(&rustle.Call{Ctx: context.Background(), Settings: s, Cache: cache, Notes: &rustle.Notes{}},
		source.Identity{Kind: "local", Source: "/clip.mp4", Key: "clip"})
	if err != nil {
		t.Fatal(err)
	}
	if again != record {
		t.Errorf("resolved %+v, want the saved record", again)
	}
	if info, err := os.Stat(root); err != nil || info.IsDir() {
		t.Errorf("the cache file became %v, %v", info, err)
	}
}
