package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/mcp"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

var toolNames = []string{"find_videos", "watch_video", "inspect_frame"}

const remoteSource = "https://www.youtube.com/watch?v=dQw4w9WgXcQ"

// recorder keeps the operations a server ran and the arguments it parsed for them.
type recorder struct {
	mu    sync.Mutex
	calls []recorded
}

type recorded struct {
	op   string
	args any
}

func (r *recorder) list() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

func fake[T any](r *recorder, name string) Op[T] {
	return func(_ *rustle.Call, args T) (*rustle.Result, error) {
		r.mu.Lock()
		r.calls = append(r.calls, recorded{name, args})
		r.mu.Unlock()
		return &rustle.Result{Meta: rustle.NewObj().Set("title", "Clip")}, nil
	}
}

func fakeOps(r *recorder) Ops {
	return Ops{Find: fake[FindArgs](r, "find"), Watch: fake[WatchArgs](r, "watch"), Frame: fake[FrameArgs](r, "frame")}
}

// reply is a JSON-RPC answer as the client reads it.
type reply struct {
	ID     *int            `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type toolResult struct {
	Content []struct {
		Type, Text, Data, MimeType string
	} `json:"content"`
	IsError *bool `json:"isError"`
}

// session is a client connected to a server over stdio pipes, as Claude Code or Codex is.
type session struct {
	t  *testing.T
	in *io.PipeWriter
	// write serializes lines, as one client process writes them.
	write sync.Mutex

	mu      sync.Mutex
	id      int
	waiting map[int]chan reply
	// answered is the id of every reply, in the order they arrived.
	answered []int
}

func connect(t *testing.T, ops Ops, client string) *session {
	t.Helper()
	// transcribe is offered only where whisper.cpp and a model are installed; without a
	// model it never is, so the tool surface doesn't depend on the machine.
	t.Setenv("RUSTLE_WHISPER_MODEL", "")
	server := &mcp.Server{Name: "rustle", Version: "test", Instructions: Instructions,
		Tools: NewServer(testSettings(t), ops).Tools(), OnError: func(error) {}}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &session{t: t, in: inW, waiting: map[int]chan reply{}}
	served, read := make(chan struct{}), make(chan struct{})
	go func() {
		server.Serve(context.Background(), inR, outW)
		outW.Close()
		close(served)
	}()
	go func() {
		defer close(read)
		lines := bufio.NewScanner(outR)
		lines.Buffer(nil, 16<<20)
		for lines.Scan() {
			var r reply
			if err := json.Unmarshal(lines.Bytes(), &r); err != nil || r.ID == nil {
				continue
			}
			s.mu.Lock()
			s.answered = append(s.answered, *r.ID)
			if ch := s.waiting[*r.ID]; ch != nil {
				ch <- r
			}
			s.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		inW.Close()
		<-served
		<-read
	})
	init := s.request("initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": client, "version": "1"}})
	if init.Error != nil {
		t.Fatalf("initialize: %s", init.Error.Message)
	}
	s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return s
}

func (s *session) send(message map[string]any) {
	s.t.Helper()
	line, err := json.Marshal(message)
	if err != nil {
		s.t.Fatal(err)
	}
	s.write.Lock()
	defer s.write.Unlock()
	if _, err := s.in.Write(append(line, '\n')); err != nil {
		s.t.Fatal(err)
	}
}

// start sends a request and returns its id and where its reply will arrive.
func (s *session) start(method string, params any) (int, <-chan reply) {
	s.mu.Lock()
	s.id++
	id := s.id
	ch := make(chan reply, 1)
	s.waiting[id] = ch
	s.mu.Unlock()
	s.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return id, ch
}

func (s *session) request(method string, params any) reply {
	s.t.Helper()
	_, ch := s.start(method, params)
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		s.t.Fatalf("no reply to %s", method)
		return reply{}
	}
}

func (s *session) call(name string, args map[string]any) toolResult {
	s.t.Helper()
	r := s.request("tools/call", map[string]any{"name": name, "arguments": args})
	if r.Error != nil {
		s.t.Fatalf("%s: %s", name, r.Error.Message)
	}
	var result toolResult
	if err := json.Unmarshal(r.Result, &result); err != nil {
		s.t.Fatal(err)
	}
	return result
}

// metaOf is the metadata line that leads a result.
func metaOf(t *testing.T, result toolResult) map[string]any {
	t.Helper()
	if len(result.Content) == 0 {
		t.Fatal("no content")
	}
	line, _, _ := strings.Cut(result.Content[0].Text, "\n")
	var meta map[string]any
	if err := json.Unmarshal([]byte(line), &meta); err != nil {
		t.Fatalf("metadata %q: %v", line, err)
	}
	return meta
}

func notesOf(t *testing.T, result toolResult) []string {
	t.Helper()
	var notes []string
	if list, ok := metaOf(t, result)["notes"].([]any); ok {
		for _, n := range list {
			notes = append(notes, n.(string))
		}
	}
	return notes
}

func TestThreeReadOnlyToolsWithLeanSchemasAndNoOutputSchemas(t *testing.T) {
	session := connect(t, fakeOps(&recorder{}), "claude-code")
	var listed struct {
		Tools []struct {
			Name         string          `json:"name"`
			Description  string          `json:"description"`
			InputSchema  json.RawMessage `json:"inputSchema"`
			OutputSchema json.RawMessage `json:"outputSchema"`
			Annotations  struct {
				ReadOnlyHint *bool `json:"readOnlyHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(session.request("tools/list", map[string]any{}).Result, &listed); err != nil {
		t.Fatal(err)
	}
	var names []string
	size := rustle.UTF16Len(Instructions)
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
		if tool.OutputSchema != nil {
			t.Errorf("%s has an output schema", tool.Name)
		}
		if tool.Annotations.ReadOnlyHint == nil || !*tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not marked read-only", tool.Name)
		}
		var schema struct {
			Required   []string                  `json:"required"`
			Properties map[string]map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		if tool.Name != "find_videos" && (len(schema.Required) == 0 || schema.Required[0] != "source") {
			t.Errorf("%s requires %v", tool.Name, schema.Required)
		}
		for name, prop := range schema.Properties {
			if _, ok := prop["title"]; ok {
				t.Errorf("%s.%s has a title", tool.Name, name)
			}
		}
		size += rustle.UTF16Len(tool.Description) + rustle.UTF16Len(string(tool.InputSchema))
	}
	equal(t, names, toolNames)
	t.Logf("tool surface: %d characters", size)
	// Every client sends this with each request. Five tools took it to 2,885; folding
	// read_transcript and listen_video into watch_video brought it to 2,454, and saying it
	// watches a frame a second by default to 2,489.
	// Raise it deliberately, not by drift.
	if size > 2500 {
		t.Errorf("tool surface grew to %d characters", size)
	}
	for _, name := range toolNames {
		if !strings.Contains(Instructions, name) {
			t.Errorf("the instructions don't name %s", name)
		}
	}
	if !strings.Contains(Instructions, "untrusted") {
		t.Error("the instructions don't call video content untrusted")
	}
}

func TestLenientArguments(t *testing.T) {
	// want is the parsed arguments the operation must get, as JSON; a TypeScript undefined is
	// the Go zero value (fps 0 is off).
	cases := []struct {
		tool, args, op, want, note string
	}{
		{"find_videos", `{"query":"cats","limit":100}`, "find", `{"limit":25}`, "limit 100 clamped to 25"},
		{"find_videos", `{"query":"cats","limit":0}`, "find", `{"limit":1}`, "limit 0 clamped to 1"},
		{"find_videos", `{"url":"https://example.com/videos","query":"cats"}`, "find", `{"url":"https://example.com/videos","query":""}`, "query and channel ignored with url"},
		{"find_videos", `{"query":"cats","limit":2.4}`, "find", `{"limit":2}`, "limit 2.4 rounded to 2"},
		{"watch_video", `{"source":"` + remoteSource + `","frames":200}`, "watch", `{"frames":120}`, "frames 200 clamped to 120"},
		{"watch_video", `{"source":"` + remoteSource + `","frames":12.5}`, "watch", `{"frames":13}`, "frames 12.5 rounded to 13"},
		{"watch_video", `{"source":"` + remoteSource + `","fps":30}`, "watch", `{"fps":8}`, "fps 30 clamped to 8"},
		{"watch_video", `{"source":"` + remoteSource + `","fps":0}`, "watch", `{"fps":0}`, "fps ignored: it must be positive"},
		{"watch_video", `{"source":"` + remoteSource + `","start_seconds":-5}`, "watch", `{"start":0}`, "start_seconds -5 clamped to 0"},
		{"watch_video", `{"source":"` + remoteSource + `","frames":0}`, "watch", `{"frames":0,"detail":"digest"}`, ""},
		{"watch_video", `{"source":"` + remoteSource + `","listen":true}`, "watch", `{"listen":"analysis","detail":"full"}`, ""},
		{"watch_video", `{"source":"` + remoteSource + `","listen":"audio"}`, "watch", `{"listen":"audio"}`, "this client may not pass audio to the model; listen=analysis can"},
		{"watch_video", `{"source":"` + remoteSource + `","listen":false}`, "watch", `{"listen":""}`, ""},
		{"watch_video", `{"source":"` + remoteSource + `","query":"cats","fps":4}`, "watch", `{"fps":0,"query":"cats"}`, "fps ignored with query: the frames are where it is said"},
		{"watch_video", `{"source":"` + remoteSource + `","frames":0,"detail":"none","query":"cats"}`, "watch", `{"query":"cats"}`, ""},
		{"watch_video", `{"source":"` + remoteSource + `","crop":[0.5,0.5,1,1]}`, "watch", `{"crop":[0.5,0.5,0.5,0.5]}`, "crop clipped to the frame: [0.5, 0.5, 0.5, 0.5]"},
		{"inspect_frame", `{"source":"` + remoteSource + `","at_seconds":3}`, "frame", `{"detail":"fit"}`, ""},
		{"inspect_frame", `{"source":"` + remoteSource + `","at_seconds":3,"crop":[0.6,0,0.6,1]}`, "frame", `{"crop":[0.6,0,0.4,1]}`, "crop clipped to the frame: [0.6, 0, 0.4, 1]"},
		{"watch_video", `{"source":"` + remoteSource + `","seconds":300,"mode":"transcript"}`, "watch", `{"start":0}`, "ignored unknown arguments: mode, seconds"},
		{"watch_video", `{"source":"` + remoteSource + `","start_seconds":"90"}`, "watch", `{"start":90}`, ""},
		// A span asks to read that part, so it gets every word; the whole video gets the digest.
		{"watch_video", `{"source":"` + remoteSource + `","start_seconds":60,"end_seconds":120}`, "watch", `{"detail":"full"}`, ""},
		// By default a call watches as a person does: a frame a second, every word.
		{"watch_video", `{"source":"` + remoteSource + `"}`, "watch", `{"fps":1,"frames":120,"detail":"full","language":"auto"}`, ""},
		// A count of frames is an overview, with the key sentences, and stays one when it
		// continues from next_start_seconds; an end asks to read that part.
		{"watch_video", `{"source":"` + remoteSource + `","frames":12}`, "watch", `{"fps":0,"frames":12,"detail":"digest"}`, ""},
		{"watch_video", `{"source":"` + remoteSource + `","frames":12,"start_seconds":600}`, "watch", `{"fps":0,"detail":"digest"}`, ""},
		{"watch_video", `{"source":"` + remoteSource + `","frames":12,"start_seconds":60,"end_seconds":120}`, "watch", `{"detail":"full"}`, ""},
		{"watch_video", `{"source":"` + remoteSource + `","query":"cats"}`, "watch", `{"fps":0,"frames":12}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.tool+" "+tc.args+" is adjusted, not rejected", func(t *testing.T) {
			r := &recorder{}
			session := connect(t, fakeOps(r), "claude-code")
			var args, want map[string]any
			if err := json.Unmarshal([]byte(tc.args), &args); err != nil {
				t.Fatal(err)
			}
			result := session.call(tc.tool, args)
			if result.IsError != nil {
				t.Fatalf("failed: %+v", result.Content)
			}
			calls := r.list()
			if len(calls) != 1 || calls[0].op != tc.op {
				t.Fatalf("ran %v, want %s", calls, tc.op)
			}
			data, _ := json.Marshal(calls[0].args)
			var got map[string]any
			json.Unmarshal(data, &got)
			json.Unmarshal([]byte(tc.want), &want)
			for key, value := range want {
				if !reflect.DeepEqual(got[key], value) {
					t.Errorf("%s = %v, want %v", key, got[key], value)
				}
			}
			if notes := notesOf(t, result); tc.note != "" && !slices.Contains(notes, tc.note) {
				t.Errorf("notes %q lack %q", notes, tc.note)
			}
		})
	}
}

func TestNullMeansTheDefault(t *testing.T) {
	r := &recorder{}
	session := connect(t, fakeOps(r), "claude-code")
	result := session.call("watch_video", map[string]any{"source": remoteSource, "frames": nil, "fps": nil, "end_seconds": nil})
	if result.IsError != nil {
		t.Fatalf("failed: %+v", result.Content)
	}
	args := r.list()[0].args.(WatchArgs)
	equal(t, args.Frames, 120)
	equal(t, args.FPS, 1.0)
	equal(t, args.End, (*float64)(nil))
	if notes, ok := metaOf(t, result)["notes"]; ok {
		t.Errorf("notes %v", notes)
	}
}

func TestMalformedInputIsOneInvalidArgumentsErrorWithARecovery(t *testing.T) {
	session := connect(t, fakeOps(&recorder{}), "claude-code")
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"watch_video", map[string]any{}},
		{"watch_video", map[string]any{"source": remoteSource, "start_seconds": 10, "end_seconds": 5}},
		{"inspect_frame", map[string]any{"source": remoteSource}},
		{"inspect_frame", map[string]any{"source": remoteSource, "at_seconds": 1, "crop": []any{2, 2, 1, 1}}},
		{"find_videos", map[string]any{}},
		{"watch_video", map[string]any{"source": remoteSource, "listen": "midi"}},
		{"watch_video", map[string]any{"source": remoteSource, "detail": "everything"}},
		{"watch_video", map[string]any{"source": remoteSource, "frames": 0, "detail": "none"}},
		{"watch_video", map[string]any{"source": remoteSource, "frames": "many"}},
	} {
		result := session.call(tc.tool, tc.args)
		if result.IsError == nil || !*result.IsError {
			t.Errorf("%s %v was not an error", tc.tool, tc.args)
			continue
		}
		var failure struct {
			Code      string `json:"code"`
			Retryable *bool  `json:"retryable"`
			Recovery  string `json:"recovery"`
		}
		if err := json.Unmarshal([]byte(result.Content[0].Text), &failure); err != nil {
			t.Fatal(err)
		}
		if failure.Code != "INVALID_ARGUMENTS" || failure.Retryable == nil || *failure.Retryable || failure.Recovery == "" {
			t.Errorf("%s %v: %s", tc.tool, tc.args, result.Content[0].Text)
		}
	}
}

func TestOperationErrorsKeepTheirCodeRetryabilityAndRecovery(t *testing.T) {
	ops := fakeOps(&recorder{})
	ops.Watch = func(*rustle.Call, WatchArgs) (*rustle.Result, error) {
		return nil, rustle.Fail("YouTube is rate limiting this connection.", "RATE_LIMITED", "Wait.").Retry(300)
	}
	session := connect(t, ops, "claude-code")
	result := session.call("watch_video", map[string]any{"source": remoteSource})
	if result.IsError == nil || !*result.IsError {
		t.Fatal("not an error")
	}
	equal(t, result.Content[0].Text,
		`{"error":"YouTube is rate limiting this connection.","code":"RATE_LIMITED","retryable":true,"recovery":"Wait.","retry_after_seconds":300}`)
}

func TestAudioOutputFollowsTheClient(t *testing.T) {
	// Claude gets analysis, others audio.
	for client, output := range map[string]string{"claude-code": "analysis", "codex-mcp-client": "audio"} {
		r := &recorder{}
		session := connect(t, fakeOps(r), client)
		session.call("watch_video", map[string]any{"source": remoteSource, "listen": true})
		if got := r.list()[0].args.(WatchArgs).Listen; got != output {
			t.Errorf("%s got %s, want %s", client, got, output)
		}
	}
}

func TestRenderPutsMetadataFirstAndSharesTheBlockWithLeadingText(t *testing.T) {
	text := Render(&rustle.Result{Meta: rustle.NewObj().Set("title", "T").Set("duration", 12.34567).Set("gap", nil),
		Content: []rustle.Block{rustle.TextBlock("0:00 hello")}}, []string{"a note"})
	equal(t, text, []rustle.Block{rustle.TextBlock(`{"title":"T","duration":12.346,"notes":["a note"]}` + "\n0:00 hello")})
	image := Render(&rustle.Result{Meta: rustle.NewObj().Set("size", []int{1, 2}),
		Content: []rustle.Block{{Type: "image", Data: "AA==", MimeType: "image/png"}}}, nil)
	equal(t, []string{image[0].Type, image[1].Type}, []string{"text", "image"})
}

func TestIdenticalCallsOnARemoteSourceAreAnsweredFromMemory(t *testing.T) {
	r := &recorder{}
	session := connect(t, fakeOps(r), "claude-code")
	session.call("find_videos", map[string]any{"query": "cats"})
	session.call("find_videos", map[string]any{"query": "cats"})
	equal(t, len(r.list()), 1)
}

func TestARememberedResultKeepsTheNotesItsOperationAdded(t *testing.T) {
	// watch_video runs again (its sheets are cached by record instead); find_videos is
	// answered from memory, and must still say what its operation noted.
	for tool, runs := range map[string]int{"watch_video": 2, "find_videos": 1} {
		count := 0
		ops := fakeOps(&recorder{})
		noting := func(c *rustle.Call) (*rustle.Result, error) {
			count++
			c.Notes.Add("the span holds 3 distinct frames, not 12")
			return &rustle.Result{Meta: rustle.NewObj()}, nil
		}
		ops.Watch = func(c *rustle.Call, _ WatchArgs) (*rustle.Result, error) { return noting(c) }
		ops.Find = func(c *rustle.Call, _ FindArgs) (*rustle.Result, error) { return noting(c) }
		session := connect(t, ops, "claude-code")
		args := map[string]any{"source": remoteSource}
		if tool == "find_videos" {
			args = map[string]any{"query": "cats"}
		}
		first := notesOf(t, session.call(tool, args))
		second := notesOf(t, session.call(tool, args))
		equal(t, second, first)
		equal(t, second, []string{"the span holds 3 distinct frames, not 12"})
		if count != runs {
			t.Errorf("%s ran its operation %d times, want %d", tool, count, runs)
		}
	}
}

func TestACancelledCallGetsNoReply(t *testing.T) {
	started, ended := make(chan struct{}), make(chan error, 1)
	ops := fakeOps(&recorder{})
	ops.Watch = func(c *rustle.Call, _ WatchArgs) (*rustle.Result, error) {
		close(started)
		select {
		case <-c.Ctx.Done():
			ended <- nil
			return nil, rustle.Cancelled()
		case <-time.After(10 * time.Second):
			ended <- context.DeadlineExceeded
			return &rustle.Result{Meta: rustle.NewObj()}, nil
		}
	}
	session := connect(t, ops, "claude-code")
	id, answer := session.start("tools/call", map[string]any{"name": "watch_video", "arguments": map[string]any{"source": remoteSource}})
	<-started
	session.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": id, "reason": "user"}})
	if err := <-ended; err != nil {
		t.Fatal("the operation never saw the cancellation")
	}
	// The server still answers, and the cancelled call's goroutine replies, if it ever does,
	// within microseconds of its operation returning.
	if r := session.request("ping", map[string]any{}); r.Error != nil {
		t.Fatalf("ping: %s", r.Error.Message)
	}
	select {
	case r := <-answer:
		t.Errorf("the cancelled call was answered: %s", r.Result)
	case <-time.After(100 * time.Millisecond):
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if slices.Contains(session.answered, id) {
		t.Errorf("replies %v include the cancelled call %d", session.answered, id)
	}
}
