package tools

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/vibeslop/rustle-mcp/internal/media"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// tempDir is a test's temporary folder with symlinks resolved, as sources resolve theirs:
// macOS hands out /var/..., which is /private/var/....
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// testSettings are the defaults, whatever the environment says, with the cache in a
// fresh folder.
func testSettings(t *testing.T) *rustle.Settings {
	t.Helper()
	s, err := rustle.LoadSettings(rustle.MapEnv(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	s.CacheDir = filepath.Join(tempDir(t), "cache")
	return s
}

// newCall is a call as the server makes one for client. Records reach the disk a moment
// after a call (SaveRecord); the cleanup, run before the cache folder's, lets them land.
func newCall(t *testing.T, s *rustle.Settings, client string) *rustle.Call {
	t.Cleanup(rustle.FlushRecords)
	return &rustle.Call{Ctx: context.Background(), Settings: s, Cache: rustle.NewCache(s.CacheDir, s.CacheBytes),
		Client: rustle.ProfileFor(client), Notes: &rustle.Notes{}}
}

// needFFmpeg skips a test that decodes when ffmpeg isn't installed.
func needFFmpeg(t *testing.T, s *rustle.Settings) media.Tools {
	t.Helper()
	tools, err := media.ToolsFor(s)
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	return tools
}

func equal[T any](t *testing.T, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
}

// failure is err as the *rustle.Error an operation must fail with.
func failure(t *testing.T, err error) *rustle.Error {
	t.Helper()
	if err == nil {
		t.Fatal("expected a rustle.Error, got none")
	}
	e, ok := rustle.AsError(err)
	if !ok {
		t.Fatalf("expected a rustle.Error, got %T: %v", err, err)
	}
	return e
}
