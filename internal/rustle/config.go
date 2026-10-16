package rustle

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type Settings struct {
	CacheDir string
	// CacheBytes is the whole cache's budget; least recently used sources are evicted past it.
	CacheBytes int64
	// MediaRoots are the folders local files may come from; empty turns local files off.
	MediaRoots []string
	Timeout    time.Duration
	// TranscriptChars is transcript characters per call, under the size at which Claude Code
	// spills output to a file.
	TranscriptChars int
	// Workers is concurrent ffmpeg processes.
	Workers int
	FFmpeg  string
	FFprobe string
	Debug   bool
}

// Env looks up an environment variable; an empty value counts as set.
type Env func(name string) (string, bool)

// ProcessEnv is the server's own environment.
func ProcessEnv(name string) (string, bool) { return os.LookupEnv(name) }

// MapEnv is an environment of fixed values, for tests.
func MapEnv(values map[string]string) Env {
	return func(name string) (string, bool) {
		v, ok := values[name]
		return v, ok
	}
}

// PathListSeparator separates folders in RUSTLE_MEDIA_ROOTS.
var PathListSeparator = string(os.PathListSeparator)

func Home() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/"
	}
	return home
}

// Expand resolves a leading ~ to the home folder.
func Expand(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		return filepath.Join(Home(), path[1:])
	}
	return path
}

// Real is the path with symlinks resolved, or made absolute when it doesn't exist.
func Real(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// TempDir is the temp folder, without the trailing slash macOS gives it.
func TempDir() string { return filepath.Clean(os.TempDir()) }

func DefaultCacheDir(env Env) string {
	if dir, ok := env("XDG_CACHE_HOME"); ok && dir != "" {
		return filepath.Join(dir, "rustle-mcp")
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(Home(), "Library", "Caches", "rustle-mcp")
	}
	return filepath.Join(Home(), ".cache", "rustle-mcp")
}

func LoadSettings(env Env) (*Settings, error) {
	number := func(name string, fallback float64) (float64, error) {
		value, ok := env("RUSTLE_" + name)
		if !ok || value == "" {
			return fallback, nil
		}
		parsed := Number(value)
		if !Finite(parsed) || parsed <= 0 {
			return 0, fmt.Errorf("RUSTLE_%s must be a positive number.", name)
		}
		return parsed, nil
	}
	// Read-only access to media under home covers what agents render and download. A
	// configured list replaces it; the temp folders and the working directory, where
	// agents render and where the client started the server, are always included.
	roots, set := env("RUSTLE_MEDIA_ROOTS")
	configured := []string{Home()}
	if set {
		configured = strings.Split(roots, PathListSeparator)
	}
	var mediaRoots []string
	if !set || roots != "" {
		cwd, _ := os.Getwd()
		seen := map[string]bool{}
		for _, p := range append(configured, TempDir(), "/tmp", cwd) {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			p = Real(Expand(p))
			if !seen[p] {
				seen[p] = true
				mediaRoots = append(mediaRoots, p)
			}
		}
	}
	s := &Settings{MediaRoots: mediaRoots}
	cache, _ := env("RUSTLE_CACHE")
	if cache == "" {
		cache = DefaultCacheDir(env)
	}
	s.CacheDir, _ = filepath.Abs(Expand(cache))
	cacheMB, err := number("CACHE_MB", 2048)
	if err != nil {
		return nil, err
	}
	s.CacheBytes = int64(cacheMB * 1024 * 1024)
	timeout, err := number("TIMEOUT", 120)
	if err != nil {
		return nil, err
	}
	s.Timeout = time.Duration(timeout * float64(time.Second))
	chars, err := number("TRANSCRIPT_CHARS", 45000)
	if err != nil {
		return nil, err
	}
	s.TranscriptChars = int(Round(chars))
	workers, err := number("WORKERS", float64(max(2, runtime.NumCPU())))
	if err != nil {
		return nil, err
	}
	s.Workers = max(1, int(Round(workers)))
	s.FFmpeg, _ = env("RUSTLE_FFMPEG")
	s.FFprobe, _ = env("RUSTLE_FFPROBE")
	debug, _ := env("RUSTLE_DEBUG")
	s.Debug = debug == "1"
	return s, nil
}

// Copy is a copy of the settings a test can change.
func (s *Settings) Copy() *Settings {
	c := *s
	c.MediaRoots = append([]string(nil), s.MediaRoots...)
	return &c
}
