package rustle

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// HashKey names a cache entry for text: 32 hex digits of its SHA-256.
func HashKey(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:32]
}

// WriteAtomic writes through a temporary file and renames it, so readers and other
// processes never see a partial file.
func WriteAtomic(path string, data []byte) error {
	var suffix [4]byte
	_, _ = rand.Read(suffix[:])
	temporary := path + "." + hex.EncodeToString(suffix[:]) + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		os.Remove(temporary)
		return err
	}
	return nil
}

// Cache is one directory per source under the cache folder. Files are replaced
// atomically, so concurrent calls and several server processes can share the folder
// without locks: the worst case is two calls computing the same file and one rename winning.
type Cache struct {
	Root   string
	budget int64

	mu           sync.Mutex
	lastEviction time.Time
}

func NewCache(root string, budget int64) *Cache { return &Cache{Root: root, budget: budget} }

// Dir is the source's folder, made if missing.
func (c *Cache) Dir(key string) (string, error) {
	path := filepath.Join(c.Root, key)
	return path, os.MkdirAll(path, 0o700)
}

func (c *Cache) Read(key, name string) []byte {
	data, err := os.ReadFile(filepath.Join(c.Root, key, name))
	if err != nil {
		return nil
	}
	return data
}

// ReadJSON decodes a cached file into v, reporting whether there was one to decode.
func (c *Cache) ReadJSON(key, name string, v any) bool {
	data := c.Read(key, name)
	return data != nil && json.Unmarshal(data, v) == nil
}

func (c *Cache) Write(key, name string, data []byte) (string, error) {
	dir, err := c.Dir(key)
	if err != nil {
		return "", err
	}
	target := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", err
	}
	return target, WriteAtomic(target, data)
}

func (c *Cache) WriteJSON(key, name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = c.Write(key, name, data)
	return err
}

// Touch marks a source as used now, for least-recently-used eviction.
func (c *Cache) Touch(key string) {
	if dir, err := c.Dir(key); err == nil {
		now := time.Now()
		_ = os.Chtimes(dir, now, now)
	}
}

var entryName = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Evict removes the least recently used sources past the budget; it runs at most once a minute.
func (c *Cache) Evict(force bool) {
	c.mu.Lock()
	if !force && time.Since(c.lastEviction) < time.Minute {
		c.mu.Unlock()
		return
	}
	c.lastEviction = time.Now()
	c.mu.Unlock()
	entries, err := os.ReadDir(c.Root)
	if err != nil {
		return
	}
	type entry struct {
		path  string
		used  time.Time
		bytes int64
	}
	var found []entry
	var total int64
	for _, e := range entries {
		if !e.IsDir() || !entryName.MatchString(e.Name()) {
			continue
		}
		path := filepath.Join(c.Root, e.Name())
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		bytes := size(path)
		found = append(found, entry{path, info.ModTime(), bytes})
		total += bytes
	}
	sort.Slice(found, func(i, j int) bool { return found[i].used.Before(found[j].used) })
	for _, e := range found {
		if total <= c.budget {
			break
		}
		os.RemoveAll(e.path)
		total -= e.bytes
	}
}

func size(path string) int64 {
	var total int64
	filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}
