package proc

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// ffmpeg processes started ahead of the calls that use them. Starting ffmpeg loads some 40
// libraries before it reads a single option: 20 ms on a busy CPU, 40 ms on one that has
// idled. A warm process has done that and waits for the options that change from call to
// call, which it reads from pipes (`-/ss pipe:3` is -ss read from pipe 3), with its input
// a link this server points at the file in use. Two decodes of a 10 s 720p clip took 65 ms
// this way and 88 ms started fresh. An unused process exits when its pipes close, which
// happens by itself when the server exits.

// late are the options read from pipes. A process waits on the first one it reads, so a
// command whose input is a file needs one before -i (-ss); one reading stdin waits on
// that instead.
var late = map[string]bool{"-ss": true, "-t": true, "-frames:v": true, "-vf": true}

const linkArg = "\x00link"

// idleLimit is the idle processes kept at most, across all commands.
const idleLimit = 8

// Shape is a command with its late values swapped for pipes and its input for a link.
type Shape struct {
	Key    string
	Args   []string
	Values []string
	Input  string
	ext    string
}

type warmProcess struct {
	p    *process
	link string
}

var warm = struct {
	sync.Mutex
	pools map[string][]*warmProcess
	// wanted is the idle processes wanted per command: seeded at startup, one for a
	// command seen in use.
	wanted map[string]int
	// broken are commands that can't start warm: ffmpeg is older than 7.0, which added -/,
	// or links have no folder.
	broken map[string]bool
	dir    string
	made   int
}{pools: map[string][]*warmProcess{}, wanted: map[string]int{}, broken: map[string]bool{}}

var warmEnabled = runtime.GOOS != "windows" && os.Getenv("RUSTLE_WARM") != "0"

var urlLike = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]+:`)

// ShapeOf is the warm shape of a command; nil when it can't run warm (URLs, several
// inputs, nothing to wait on).
func ShapeOf(command string, args []string) *Shape {
	if !warmEnabled {
		return nil
	}
	var out, values []string
	input := ""
	inputs := 0
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case late[arg] && i+1 < len(args):
			// Not /dev/fd/N: a pipe opened by that path on Linux is a new reader, not the one ffmpeg was given.
			out = append(out, "-/"+arg[1:], "pipe:"+strconv.Itoa(3+len(values)))
			i++
			values = append(values, args[i])
		case arg == "-i" && i+1 < len(args):
			i++
			path := args[i]
			inputs++
			if inputs > 1 || (path != "pipe:0" && urlLike.MatchString(path)) {
				return nil
			}
			if path == "pipe:0" {
				out = append(out, "-i", path)
			} else if len(values) == 0 {
				return nil
			} else {
				input, _ = filepath.Abs(path)
				out = append(out, "-i", linkArg)
			}
		default:
			out = append(out, arg)
		}
	}
	if len(values) == 0 {
		return nil
	}
	ext := ""
	if input != "" {
		ext = strings.ToLower(filepath.Ext(input))
	}
	return &Shape{Key: command + "\x00" + ext + "\x00" + strings.Join(out, "\x00"), Args: out, Values: values, Input: input, ext: ext}
}

// start runs with warm locked.
func start(command string, s *Shape) (*warmProcess, error) {
	link := ""
	if s.Input != "" {
		if warm.dir == "" {
			sweep()
			dir, err := os.MkdirTemp(rustle.TempDir(), fmt.Sprintf("rustle-%d-", os.Getpid()))
			if err != nil {
				return nil, err
			}
			warm.dir = dir
		}
		// A file's extension can decide how ffmpeg reads it, so the link keeps it.
		link = filepath.Join(warm.dir, strconv.Itoa(warm.made)+s.ext)
		warm.made++
	}
	args := make([]string, len(s.Args))
	for i, a := range s.Args {
		if a == linkArg {
			a = link
		}
		args[i] = a
	}
	p, err := startProcess(command, args, len(s.Values))
	if err != nil {
		return nil, err
	}
	if link != "" {
		go func() {
			<-p.exited
			os.Remove(link)
		}()
	}
	return &warmProcess{p: p, link: link}, nil
}

// sweep removes link folders of servers that were killed before they could remove their own.
func sweep() {
	entries, err := os.ReadDir(rustle.TempDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "rustle-") {
			continue
		}
		rest := name[len("rustle-"):]
		dash := strings.IndexByte(rest, '-')
		if dash <= 0 {
			continue
		}
		pid, err := strconv.Atoi(rest[:dash])
		if err != nil || pid <= 0 || alive(pid) {
			continue
		}
		path := filepath.Join(rustle.TempDir(), name)
		if info, err := os.Lstat(path); err == nil && info.IsDir() && owned(info) {
			os.RemoveAll(path)
		}
	}
}

// Cleanup removes this server's link folder; the server calls it as it exits.
func Cleanup() {
	warm.Lock()
	defer warm.Unlock()
	if warm.dir != "" {
		os.RemoveAll(warm.dir)
	}
}

func idle() int {
	count := 0
	for _, pool := range warm.pools {
		count += len(pool)
	}
	return count
}

func fill(command string, s *Shape) {
	warm.Lock()
	defer warm.Unlock()
	if warm.broken[s.Key] {
		return
	}
	for len(warm.pools[s.Key]) < warm.wanted[s.Key] && idle() < idleLimit {
		w, err := start(command, s)
		if err != nil {
			// No temp folder for links, or no ffmpeg: calls start ffmpeg fresh.
			warm.broken[s.Key] = true
			return
		}
		warm.pools[s.Key] = append(warm.pools[s.Key], w)
	}
}

// unwarm stops starting this command's shape warm, and ends the processes waiting for it:
// it failed warm and then ran fresh. An ffmpeg before 7.0 exits on -/, but can be handed
// to a call before it has, so idling alone doesn't find it out.
func unwarm(command string, args []string) {
	s := ShapeOf(command, args)
	if s == nil {
		return
	}
	warm.Lock()
	defer warm.Unlock()
	warm.broken[s.Key] = true
	for _, w := range warm.pools[s.Key] {
		terminate(w.p)
		w.p.closeParent()
	}
	delete(warm.pools, s.Key)
}

// Prewarm keeps count processes of this command's shape waiting.
func Prewarm(command string, args []string, count int) {
	s := ShapeOf(command, args)
	if s == nil {
		return
	}
	warm.Lock()
	warm.wanted[s.Key] = max(warm.wanted[s.Key], count)
	warm.Unlock()
	fill(command, s)
}

// takeWarm is a process already running `command args`, released by writing its late
// values; nil when none waits. It is replaced once it exits, so the next call finds one
// too without this call's decode sharing the CPU with a starting ffmpeg.
func takeWarm(command string, args []string) *process {
	s := ShapeOf(command, args)
	if s == nil {
		return nil
	}
	warm.Lock()
	if _, ok := warm.wanted[s.Key]; !ok {
		warm.wanted[s.Key] = 1
	}
	var taken *warmProcess
	for len(warm.pools[s.Key]) > 0 {
		w := warm.pools[s.Key][0]
		warm.pools[s.Key] = warm.pools[s.Key][1:]
		if !w.p.done() {
			taken = w
			break
		}
		// Exited while idle: this ffmpeg can't read options from pipes. One killed by a
		// signal (out of memory, pkill) says nothing about ffmpeg, but must not be handed
		// out: it would never answer.
		if w.p.cmd.ProcessState.Exited() {
			warm.broken[s.Key] = true
		}
		w.p.closeParent()
	}
	warm.Unlock()
	refill := func() { time.AfterFunc(250*time.Millisecond, func() { fill(command, s) }) }
	if taken == nil {
		refill()
		return nil
	}
	if taken.link != "" {
		if err := os.Symlink(s.Input, taken.link); err != nil {
			taken.p.kill()
			taken.p.closeParent()
			return nil
		}
	}
	// Started later, a replacement doesn't share the CPU with this call's last steps.
	go func() {
		<-taken.p.exited
		refill()
	}()
	for k, value := range s.Values {
		// Writing to a process that has died fails; the call then sees it exit.
		_, _ = taken.p.extra[k].WriteString(value)
		taken.p.extra[k].Close()
	}
	return taken.p
}

// TakeWarm hands out a waiting process for tests, which drive its pipes themselves.
func TakeWarm(command string, args []string) (stdin, stdout *os.File, exited <-chan struct{}, code func() int, ok bool) {
	p := takeWarm(command, args)
	if p == nil {
		return nil, nil, nil, nil, false
	}
	go func() {
		<-p.exited
		p.stderr.Close()
	}()
	return p.stdin, p.stdout, p.exited, p.code, true
}
