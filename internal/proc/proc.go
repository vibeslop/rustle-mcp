// Package proc runs ffmpeg and friends: bounded output, deadlines, cancellation, and
// processes started ahead of the calls that use them (warm.go).
package proc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

type Options struct {
	Input   []byte
	Timeout time.Duration
	// MaxBytes of stdout are kept before the process is killed and the call fails.
	MaxBytes int
	// OnStdout takes stdout as it arrives instead of returning it. The chunk is only valid
	// during the call.
	OnStdout func(chunk []byte)
	// Warm uses a process started ahead of time if one waits (see warm.go).
	Warm bool
	// Until reports whether the output is complete: the call then returns with code 0
	// without waiting the 2-3 ms ffmpeg takes to shut down, which then happens on its own.
	// last is the end of the latest stdout chunk.
	Until func(bytes int, last []byte, stderr string) bool
}

type Result struct {
	Stdout []byte
	Stderr string
	// Code is the exit code, -1 when a signal ended the process.
	Code int
}

// Which finds a program on PATH, or in the usual Homebrew and system folders that MCP
// clients' minimal PATH misses.
func Which(name string) string {
	dirs := append(filepath.SplitList(os.Getenv("PATH")), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/snap/bin")
	suffixes := []string{""}
	if runtime.GOOS == "windows" {
		suffixes = []string{".exe", ""}
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		for _, suffix := range suffixes {
			candidate := filepath.Join(dir, name+suffix)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	return ""
}

// Semaphore admits at most its size holders at once, in arrival order.
type Semaphore struct{ slots chan struct{} }

func NewSemaphore(size int) *Semaphore { return &Semaphore{slots: make(chan struct{}, max(1, size))} }

// Use runs fn holding a slot; a caller cancelled while queued leaves without one.
func Use[T any](s *Semaphore, ctx context.Context, fn func() (T, error)) (T, error) {
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		var zero T
		return zero, rustle.Cancelled()
	}
	defer func() { <-s.slots }()
	return fn()
}

var trace = os.Getenv("RUSTLE_TRACE") == "1"

// Run runs a program to completion; stderr is kept (bounded) for diagnostics, never shown raw.
func Run(ctx context.Context, command string, args []string, o Options) (Result, error) {
	if !trace {
		return warmRun(ctx, command, args, o)
	}
	started := time.Now()
	result, err := warmRun(ctx, command, args, o)
	var shown []string
	for _, a := range args {
		if !strings.HasPrefix(a, "/") {
			shown = append(shown, a)
		}
	}
	line := strings.Join(shown, " ")
	if len(line) > 160 {
		line = line[:160]
	}
	fmt.Fprintf(os.Stderr, "rustle trace %dms %s %s\n", time.Since(started).Milliseconds(), filepath.Base(command), line)
	return result, err
}

// warmRun runs a warm process if one waits. One that fails is run again from scratch, so
// it can't fail where a fresh one wouldn't.
func warmRun(ctx context.Context, command string, args []string, o Options) (Result, error) {
	var p *process
	if o.Warm && ctx.Err() == nil {
		p = takeWarm(command, args)
	}
	if p == nil {
		return spawnRun(ctx, command, args, o, nil)
	}
	var streamed atomic.Bool
	opts := o
	if o.OnStdout != nil {
		opts.OnStdout = func(chunk []byte) {
			streamed.Store(true)
			o.OnStdout(chunk)
		}
	}
	result, err := spawnRun(ctx, command, args, opts, p)
	if err != nil || result.Code == 0 || streamed.Load() {
		return result, err
	}
	fresh, err := spawnRun(ctx, command, args, o, nil)
	if err == nil && fresh.Code == 0 {
		unwarm(command, args)
	}
	return fresh, err
}

// process is a started program with its pipes. Its stdio ends are files the parent owns,
// so waiting for the exit never closes a pipe that still holds unread output.
type process struct {
	cmd    *exec.Cmd
	stdin  *os.File
	stdout *os.File
	stderr *os.File
	// extra are the write ends of pipes 3 and up.
	extra  []*os.File
	exited chan struct{}
}

func startProcess(command string, args []string, extra int) (*process, error) {
	cmd := exec.Command(command, args...)
	var parent, child []*os.File
	fail := func(err error) (*process, error) {
		for _, f := range append(parent, child...) {
			f.Close()
		}
		return nil, err
	}
	// Pipe 0 is stdin, which the child reads; the others it writes, except the extra
	// pipes from 3 up, which carry option values to it.
	for i := range 3 + extra {
		r, w, err := os.Pipe()
		if err != nil {
			return fail(err)
		}
		if i == 1 || i == 2 {
			parent, child = append(parent, r), append(child, w)
		} else {
			parent, child = append(parent, w), append(child, r)
		}
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = child[0], child[1], child[2]
	cmd.ExtraFiles = child[3:]
	err := cmd.Start()
	for _, f := range child {
		f.Close()
	}
	child = nil
	if err != nil {
		return fail(err)
	}
	p := &process{cmd: cmd, stdin: parent[0], stdout: parent[1], stderr: parent[2], extra: parent[3:], exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *process) kill() { _ = p.cmd.Process.Kill() }

func (p *process) done() bool {
	select {
	case <-p.exited:
		return true
	default:
		return false
	}
}

func (p *process) code() int { return p.cmd.ProcessState.ExitCode() }

func spawnRun(ctx context.Context, command string, args []string, o Options, p *process) (Result, error) {
	// Decodes queued behind a semaphore start after a cancel.
	if ctx.Err() != nil {
		if p != nil {
			p.kill()
			p.closeParent()
		}
		return Result{}, rustle.Cancelled()
	}
	if p == nil {
		var err error
		if p, err = startProcess(command, args, 0); err != nil {
			return Result{}, err
		}
	}
	// mu guards the output and is held while OnStdout runs, so no chunk is delivered
	// after the call has returned.
	var (
		mu      sync.Mutex
		stdout  []byte
		size    int
		last    []byte
		stderr  strings.Builder
		failure error
		settled bool
		early   = make(chan struct{})
		readers sync.WaitGroup
	)
	kill := func(err error) {
		mu.Lock()
		if failure == nil {
			failure = err
		}
		mu.Unlock()
		p.kill()
	}
	// check runs with mu held.
	check := func() {
		if failure == nil && !settled && o.Until != nil && o.Until(size, last, stderr.String()) {
			settled = true
			close(early)
		}
	}
	readers.Add(2)
	go func() {
		defer readers.Done()
		buf := make([]byte, 64<<10)
		for {
			n, err := p.stdout.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				mu.Lock()
				size += n
				if o.MaxBytes > 0 && size > o.MaxBytes {
					if failure == nil {
						failure = rustle.Fail("The decoded output exceeded its size limit.", "RESPONSE_TOO_LARGE",
							"Request a shorter span, a crop or fewer frames.")
					}
					mu.Unlock()
					p.kill()
					continue
				}
				if !settled && failure == nil {
					if o.OnStdout != nil {
						o.OnStdout(chunk)
					} else {
						stdout = append(stdout, chunk...)
					}
				}
				last = append(last[:0], chunk[max(0, n-64):]...)
				check()
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		defer readers.Done()
		buf := make([]byte, 16<<10)
		for {
			n, err := p.stderr.Read(buf)
			if n > 0 {
				mu.Lock()
				if stderr.Len() < 65536 {
					stderr.Write(buf[:n])
				}
				check()
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		// The child may exit before reading all its input.
		if len(o.Input) > 0 {
			_, _ = p.stdin.Write(o.Input)
		}
		p.stdin.Close()
	}()
	var timer *time.Timer
	if o.Timeout > 0 {
		timer = time.AfterFunc(o.Timeout, func() {
			kill(rustle.Fail("The media operation took too long and was stopped.", "OPERATION_TIMEOUT",
				"Request fewer frames or a shorter span."))
		})
	}
	closed := make(chan struct{})
	go func() {
		readers.Wait()
		<-p.exited
		p.stdout.Close()
		p.stderr.Close()
		// A call that returned early leaves its deadline running until the process ends.
		if timer != nil {
			timer.Stop()
		}
		close(closed)
	}()
	stop := context.AfterFunc(ctx, func() { kill(rustle.Cancelled()) })
	finished := false
	select {
	case <-closed:
		finished = true
	case <-early:
	}
	stop()
	mu.Lock()
	defer mu.Unlock()
	if failure != nil {
		return Result{}, failure
	}
	settled = true
	code := 0
	if finished {
		code = p.code()
	}
	return Result{Stdout: stdout, Stderr: stderr.String(), Code: code}, nil
}

func (p *process) closeParent() {
	p.stdin.Close()
	for _, f := range p.extra {
		f.Close()
	}
	go func() {
		<-p.exited
		p.stdout.Close()
		p.stderr.Close()
	}()
}
