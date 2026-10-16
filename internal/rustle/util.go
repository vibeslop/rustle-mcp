package rustle

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Round rounds halves up, toward positive infinity: -2.5 rounds to -2, unlike math.Round.
// Times and sizes the model reads are rounded this way.
func Round(v float64) float64 {
	// Inlined with a product as its argument, v - r could fuse into one multiply-add that
	// skips the product's rounding (arm64 does), and Round(0.0055*1000) would be 5. The
	// conversion rounds the product first, as the language spec promises.
	v = float64(v)
	r := math.Floor(v)
	if v-r >= 0.5 {
		r++
	}
	return r
}

// MS rounds to milliseconds.
func MS(v float64) float64 { return Round(v*1000) / 1000 }

// Short prints 12.5 as "12.5" and 3 as "3"; at most three decimals.
func Short(v float64) string { return NumString(MS(v)) }

// Fixed writes v with a fixed number of decimals, as ffmpeg's options take times.
func Fixed(v float64, decimals int) string { return strconv.FormatFloat(v, 'f', decimals, 64) }

// Clock is m:ss, or h:mm:ss from one hour, as transcripts print time.
func Clock(seconds float64) string {
	total := int64(math.Floor(math.Max(0, seconds)))
	h, m, s := total/3600, total%3600/60, total%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// LabelTime is m:ss.ss (or h:mm:ss.ss), rounded like the metadata so a label never disagrees with it.
func LabelTime(seconds float64, decimals int) string {
	scale := math.Pow(10, float64(decimals))
	total := int64(Round(math.Max(0, seconds) * scale))
	whole := total / int64(scale)
	if decimals == 0 {
		return Clock(float64(whole))
	}
	return Clock(float64(whole)) + "." + fmt.Sprintf("%0*d", decimals, total%int64(scale))
}

// Clamp bounds value to [low, high], noting a change.
func Clamp(name string, value, low, high float64, notes *Notes) float64 {
	bounded := math.Min(math.Max(value, low), high)
	if bounded != value {
		notes.Add(fmt.Sprintf("%s %s clamped to %s", name, Short(value), Short(bounded)))
	}
	return bounded
}

// Whole clamps and rounds to an integer, noting each change.
func Whole(name string, value, low, high float64, notes *Notes) int {
	bounded := Clamp(name, value, low, high, notes)
	rounded := math.Floor(bounded + 0.5)
	if rounded != bounded {
		notes.Add(fmt.Sprintf("%s %s rounded to %d", name, Short(bounded), int(rounded)))
	}
	return int(rounded)
}

func B64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

// Linspace is count evenly spaced values from a to b inclusive.
func Linspace(a, b float64, count int) []float64 {
	if count <= 1 {
		return []float64{a}
	}
	out := make([]float64, count)
	for i := range out {
		out[i] = a + (b-a)*float64(i)/float64(count-1)
	}
	return out
}

// Notes are short remarks about clamped or ignored arguments, shown to the model in the
// metadata. Concurrent parts of one call add to the same list.
type Notes struct {
	mu   sync.Mutex
	list []string
}

func (n *Notes) Add(notes ...string) {
	n.mu.Lock()
	n.list = append(n.list, notes...)
	n.mu.Unlock()
}

func (n *Notes) Len() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.list)
}

func (n *Notes) List() []string { return n.Since(0) }

// Since is the notes added after the first `from`.
func (n *Notes) Since(from int) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if from >= len(n.list) {
		return []string{}
	}
	return append([]string{}, n.list[from:]...)
}

// MapLimit runs fn over items with at most limit in flight, keeping order. Like
// Promise.all, the first error is returned, once every started item has finished.
func MapLimit[T, R any](items []T, limit int, fn func(item T, index int) (R, error)) ([]R, error) {
	results := make([]R, len(items))
	if len(items) == 0 {
		return results, nil
	}
	limit = max(1, min(limit, len(items)))
	var (
		mu    sync.Mutex
		next  int
		first error
		wg    sync.WaitGroup
	)
	for range limit {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if next >= len(items) || first != nil {
					mu.Unlock()
					return
				}
				i := next
				next++
				mu.Unlock()
				r, err := fn(items[i], i)
				mu.Lock()
				if err != nil && first == nil {
					first = err
				}
				results[i] = r
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return results, first
}

// Flight runs one function per key at a time: concurrent callers share its result.
type Flight[T any] struct {
	mu      sync.Mutex
	running map[string]*flight[T]
}

type flight[T any] struct {
	done  chan struct{}
	value T
	err   error
}

// Do runs fn for key, or joins the run already going. The run continues for the others
// when a caller's ctx ends; that caller gets Cancelled.
func (f *Flight[T]) Do(ctx context.Context, key string, fn func() (T, error)) (T, error) {
	f.mu.Lock()
	if f.running == nil {
		f.running = map[string]*flight[T]{}
	}
	run := f.running[key]
	if run == nil {
		run = &flight[T]{done: make(chan struct{})}
		f.running[key] = run
		go func() {
			defer func() {
				f.mu.Lock()
				delete(f.running, key)
				f.mu.Unlock()
				close(run.done)
			}()
			run.value, run.err = fn()
		}()
	}
	f.mu.Unlock()
	return Wait(ctx, run.done, func() (T, error) { return run.value, run.err })
}

// Running is the run for key in progress, if any.
func (f *Flight[T]) Running(key string) (<-chan struct{}, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if run := f.running[key]; run != nil {
		return run.done, true
	}
	return nil, false
}

// Wait returns result() once done closes, or Cancelled when ctx ends first.
func Wait[T any](ctx context.Context, done <-chan struct{}, result func() (T, error)) (T, error) {
	select {
	case <-done:
		return result()
	default:
	}
	select {
	case <-done:
		return result()
	case <-ctx.Done():
		var zero T
		return zero, Cancelled()
	}
}

// Detach runs work without ctx's cancellation and waits for it, or for ctx to end: shared
// work runs on for the other callers.
func Detach[T any](ctx context.Context, work func(ctx context.Context) (T, error)) (T, error) {
	done := make(chan struct{})
	var value T
	var err error
	go func() {
		defer close(done)
		value, err = work(context.WithoutCancel(ctx))
	}()
	return Wait(ctx, done, func() (T, error) { return value, err })
}

// UTF16Len is a string's length in UTF-16 code units, the unit transcript budgets and
// costs are measured in.
func UTF16Len(s string) int {
	n := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < 0x80 {
			n++
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
		i += size
	}
	return n
}

// SpaceClass is the white space Rustle collapses and trims, for use inside a regexp
// character class: ASCII spaces, no-break and Unicode spaces, line separators and the
// byte-order mark. Go's \s is ASCII only.
const SpaceClass = `\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

// IsSpace reports whether r is in SpaceClass.
func IsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// TrimSpace trims SpaceClass from both ends; unlike strings.TrimSpace it also takes a
// byte-order mark and leaves U+0085.
func TrimSpace(s string) string { return strings.TrimFunc(s, IsSpace) }

// CollapseSpace replaces each run of white space with one space and trims, as
// .replace(/\s+/g, ' ').trim() does.
func CollapseSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// Cut is at most count UTF-16 units of text, never splitting a surrogate pair.
func Cut(text string, count int) string {
	n := 0
	for i, r := range text {
		width := 1
		if r >= 0x10000 {
			width = 2
		}
		if n+width > count {
			return text[:i]
		}
		n += width
	}
	return text
}

// Number reads a JSON value as a number, as YouTube and ffprobe write numbers as strings:
// strings are trimmed and parsed, with "" as 0 and 0x as hex; anything unparseable, and a
// missing value, is NaN.
func Number(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case bool:
		if v {
			return 1
		}
		return 0
	case string:
		s := TrimSpace(v)
		if s == "" {
			return 0
		}
		if len(s) > 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
			n, err := strconv.ParseUint(s[2:], 16, 64)
			if err != nil {
				return math.NaN()
			}
			return float64(n)
		}
		switch s {
		case "Infinity", "+Infinity":
			return math.Inf(1)
		case "-Infinity":
			return math.Inf(-1)
		}
		for _, c := range s {
			// ParseFloat also reads "inf", "nan", hex floats and underscores, which Number refuses.
			if !(c >= '0' && c <= '9' || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-') {
				return math.NaN()
			}
		}
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
				return n
			}
			return math.NaN()
		}
		return n
	}
	return math.NaN()
}

// Finite reports whether v is a finite number.
func Finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
