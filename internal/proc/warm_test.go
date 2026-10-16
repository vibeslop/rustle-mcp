package proc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// helperEnv selects what TestHelperProcess does when the test binary runs itself as a
// separate server process.
const helperEnv = "RUSTLE_WARM_HELPER"

var (
	ffmpeg string
	// video is 3 s at 10 fps with a keyframe every second.
	video string
	// old stands for an ffmpeg before 7.0: it exits on the -/ options a warm process waits
	// on. Written before any test starts a process, so no fork can hold it open for writing.
	old string
)

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "" {
		os.Exit(m.Run())
	}
	os.Exit(run(m))
}

func run(m *testing.M) int {
	// Warm processes leave this process's link folder behind otherwise.
	defer Cleanup()
	ffmpeg = Which("ffmpeg")
	if ffmpeg == "" {
		return m.Run()
	}
	dir, err := os.MkdirTemp("", "warm-test-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	dir, _ = filepath.EvalSymlinks(dir)
	video = filepath.Join(dir, "clip.mp4")
	if out, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=3",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "10", "-pix_fmt", "yuv420p", video).CombinedOutput(); err != nil {
		panic(fmt.Sprintf("ffmpeg: %v %s", err, out))
	}
	old = filepath.Join(dir, "old-ffmpeg")
	if err := os.WriteFile(old, []byte("#!/bin/sh\ncase \"$*\" in *-/ss*) sleep 0.3; exit 1;; esac\necho decoded\n"), 0o755); err != nil {
		panic(err)
	}
	return m.Run()
}

func needsWarm(t *testing.T) {
	t.Helper()
	if ffmpeg == "" {
		t.Skip("needs ffmpeg")
	}
	if !warmEnabled {
		t.Skip("processes don't start warm here")
	}
}

// readsOptionFiles: ffmpeg 7 added -/, an option read from a file; older ones exit, and
// calls start fresh instead.
func readsOptionFiles(t *testing.T) bool {
	value := filepath.Join(t.TempDir(), "one")
	if err := os.WriteFile(value, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	return exec.Command(ffmpeg, "-v", "error", "-/t", value, "-f", "lavfi", "-i", "nullsrc", "-frames:v", "1", "-f", "null", "-").Run() == nil
}

// decode is two frames from `at`, the way frames.go asks for them.
func decode(at string) []string { return decodeFrom(at, video, "0") }

func decodeFrom(at, input, threads string) []string {
	return []string{"-hide_banner", "-nostdin", "-loglevel", "info", "-threads", threads, "-ss", at, "-t", "1", "-copyts",
		"-protocol_whitelist", "file,crypto,data", "-i", input, "-map", "0:V:0", "-frames:v", "2",
		"-vf", "scale=32:18,showinfo=checksum=0", "-pix_fmt", "rgb24", "-f", "rawvideo", "pipe:1"}
}

const twoFrames = 2 * 32 * 18 * 3

// release ends a process a test took but didn't expect.
func release(p *process) {
	if p != nil {
		p.kill()
		p.closeParent()
	}
}

func TestACommandRunsWarmOnlyIfItWaitsBeforeOpeningItsInput(t *testing.T) {
	if !warmEnabled {
		t.Skip("processes don't start warm here")
	}
	input := filepath.Join(t.TempDir(), "clip.mp4")
	if s := ShapeOf("ffmpeg", decodeFrom("1", input, "0")); s == nil || !reflect.DeepEqual(s.Values, []string{"1", "1", "2", "scale=32:18,showinfo=checksum=0"}) {
		t.Errorf("shape %+v", s)
	}
	if s := ShapeOf("ffmpeg", decodeFrom("1", "https://example.com/v.mp4", "0")); s != nil {
		t.Errorf("a URL input has shape %+v", s)
	}
	// Without -ss the input would open, through a link not yet made, as the process starts.
	if s := ShapeOf("ffmpeg", []string{"-i", input, "-vf", "null", "-f", "null", "-"}); s != nil {
		t.Errorf("an input with nothing before it has shape %+v", s)
	}
	if s := ShapeOf("ffmpeg", []string{"-f", "h264", "-i", "pipe:0", "-vf", "null", "-f", "null", "-"}); s == nil || !reflect.DeepEqual(s.Values, []string{"null"}) {
		t.Errorf("stdin input: shape %+v", s)
	}
	// Values differ from call to call; the command they fill in is the same.
	if a, b := ShapeOf("ffmpeg", decodeFrom("0", input, "0")), ShapeOf("ffmpeg", decodeFrom("2.5", input, "0")); a == nil || b == nil || a.Key != b.Key {
		t.Errorf("keys differ: %+v, %+v", a, b)
	}
}

func TestAWarmProcessDecodesWhatAFreshOneDoes(t *testing.T) {
	needsWarm(t)
	ctx := context.Background()
	Prewarm(ffmpeg, decode("0"), 1)
	warm, err := Run(ctx, ffmpeg, decode("1.2"), Options{Warm: true})
	if err != nil {
		t.Fatal(err)
	}
	// The waiting process was used, so none is left until the replacement starts.
	if p := takeWarm(ffmpeg, decode("0")); p != nil {
		release(p)
		t.Error("a warm process was still waiting")
	}
	fresh, err := Run(ctx, ffmpeg, decode("1.2"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if warm.Code != 0 || len(warm.Stdout) != twoFrames {
		t.Fatalf("warm: code %d, %d bytes; stderr %s", warm.Code, len(warm.Stdout), warm.Stderr)
	}
	if !bytes.Equal(warm.Stdout, fresh.Stdout) {
		t.Error("warm and fresh decodes differ")
	}
	if !regexp.MustCompile(`n:\s*1\s+pts:`).MatchString(warm.Stderr) {
		t.Errorf("no second frame logged: %s", warm.Stderr)
	}
}

func TestAWarmProcessIsStillWaitingWhenTakenAfterIdling(t *testing.T) {
	needsWarm(t)
	if !readsOptionFiles(t) {
		t.Skip("needs ffmpeg 7 or newer")
	}
	// A command of its own (one thread), so the replacement the test above schedules isn't taken here.
	Prewarm(ffmpeg, decodeFrom("0", video, "1"), 1)
	// Linux can't open a socket as /dev/fd/N, so a process reading its options there died at once.
	time.Sleep(500 * time.Millisecond)
	p := takeWarm(ffmpeg, decodeFrom("1.2", video, "1"))
	if p == nil {
		t.Fatal("no warm process survived idling")
	}
	go io.Copy(io.Discard, p.stderr)
	p.stdin.Close()
	out, err := io.ReadAll(p.stdout)
	if err != nil {
		t.Fatal(err)
	}
	<-p.exited
	p.stdout.Close()
	p.stderr.Close()
	if p.code() != 0 || len(out) != twoFrames {
		t.Errorf("code %d, %d bytes", p.code(), len(out))
	}
}

func TestAnIdleProcessKilledByASignalIsSkippedNotHandedToACall(t *testing.T) {
	needsWarm(t)
	marker := fmt.Sprintf("rustle-killed-%d", os.Getpid())
	args := func(at string) []string {
		a := decode(at)
		return append(append(a[:len(a)-1:len(a)-1], "-metadata", "comment="+marker), "pipe:1")
	}
	Prewarm(ffmpeg, args("0"), 1)
	time.Sleep(300 * time.Millisecond)
	_ = exec.Command("pkill", "-KILL", "-f", marker).Run()
	time.Sleep(200 * time.Millisecond)
	// Handed out, the dead process never closes again, and the call never settles.
	result, err := Run(context.Background(), ffmpeg, args("1.2"), Options{Warm: true, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if result.Code != 0 || len(result.Stdout) != twoFrames {
		t.Errorf("code %d, %d bytes", result.Code, len(result.Stdout))
	}
}

func TestACommandThatFailsWarmButRunsFreshIsNotStartedWarmAgain(t *testing.T) {
	needsWarm(t)
	args := []string{"-ss", "1", "-i", video, "pipe:1"}
	Prewarm(old, args, 1)
	result, err := Run(context.Background(), old, args, Options{Warm: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Stdout) != "decoded\n" {
		t.Errorf("stdout %q", result.Stdout)
	}
	time.Sleep(400 * time.Millisecond)
	if p := takeWarm(old, args); p != nil {
		release(p)
		t.Error("a replacement was started")
	}
}

// helper runs this test binary as a separate server process with TMPDIR set, and returns
// what it printed.
func helper(t *testing.T, scenario, tmpdir string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), helperEnv+"="+scenario, "TMPDIR="+tmpdir, "RUSTLE_TEST_VIDEO="+video)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper: %v %s", err, stderr.String())
	}
	return string(out) + stderr.String()
}

func TestHelperProcess(t *testing.T) {
	scenario := os.Getenv(helperEnv)
	if scenario == "" {
		return
	}
	ffmpeg := Which("ffmpeg")
	args := decodeFrom("1", os.Getenv("RUSTLE_TEST_VIDEO"), "0")
	Prewarm(ffmpeg, args, 1)
	if scenario == "fresh" {
		result, err := Run(context.Background(), ffmpeg, args, Options{Warm: true})
		if err != nil {
			fmt.Println("error", err)
		} else {
			fmt.Println("result", result.Code, len(result.Stdout))
		}
	}
	Cleanup()
	os.Exit(0)
}

func TestWithoutATempFolderForLinksCallsStartFFmpegFresh(t *testing.T) {
	needsWarm(t)
	// The server stays up, and the call is answered.
	out := helper(t, "fresh", filepath.Join(filepath.Dir(video), "missing"))
	if want := fmt.Sprintf("result 0 %d", twoFrames); !strings.Contains(out, want) {
		t.Errorf("helper printed %q, want %q", out, want)
	}
}

func TestAServerThatWasKilledLeavesNoLinkFolderBehindForLong(t *testing.T) {
	needsWarm(t)
	// A folder named for a process that has exited, as a killed server leaves it.
	exited := exec.Command("true")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	stale, err := os.MkdirTemp("", fmt.Sprintf("rustle-%d-", exited.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stale)
	mine, err := os.MkdirTemp("", fmt.Sprintf("rustle-%d-", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(mine)
	helper(t, "sweep", os.TempDir())
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the dead server's folder is still there")
	}
	if _, err := os.Stat(mine); err != nil {
		t.Errorf("a live server's folder is gone: %v", err)
	}
}
