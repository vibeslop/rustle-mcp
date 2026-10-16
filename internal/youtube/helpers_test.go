package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vibeslop/rustle-mcp/internal/fetch"
	"github.com/vibeslop/rustle-mcp/internal/jsonv"
	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// M and A write JSON fixtures as Go literals.
type (
	M = map[string]any
	A = []any
)

// newCall is a call with a cache folder of its own. It also forgets what earlier tests,
// or earlier runs under -count, left in the package's memory: the visitor id, WEB
// storyboard specs, channel ids, streams and indexes.
func newCall(t *testing.T) *rustle.Call {
	t.Helper()
	settings, err := rustle.LoadSettings(rustle.MapEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	settings.CacheDir = t.TempDir()
	visitor.Lock()
	visitor.known = nil
	visitor.Unlock()
	specs.Lock()
	clear(specs.fetched)
	specs.Unlock()
	browseIDs.Clear()
	state.Lock()
	clear(state.streams)
	clear(state.indexes)
	state.Unlock()
	return &rustle.Call{
		Ctx: context.Background(), Settings: settings, Cache: rustle.NewCache(settings.CacheDir, settings.CacheBytes), Notes: &rustle.Notes{},
	}
}

// parse is v as the jsonv value a response carrying it would parse to.
func parse(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jsonv.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// merge is the objects' keys in one new object, later ones winning, as {...a, ...b} is.
func merge(objects ...M) M {
	out := M{}
	for _, o := range objects {
		for k, v := range o {
			out[k] = v
		}
	}
	return out
}

// request is one request the stub answered, with its JSON body decoded.
type request struct {
	URL    string
	Method string
	Header http.Header
	Body   M
}

// client is the InnerTube client a request was sent as.
func (r request) client() string {
	name, _ := dig(r.Body, "context", "client", "clientName").(string)
	return name
}

// reply is an answer with a status of its own.
type reply struct {
	status int
	body   []byte
}

func status(code int) reply { return reply{code, []byte("{}")} }

// stub answers requests with handler, recording each. A handler returns a reply, a body
// as a string or bytes, or a value sent as JSON.
type stub struct {
	mu      sync.Mutex
	calls   []request
	handler func(request) any
}

func (s *stub) RoundTrip(req *http.Request) (*http.Response, error) {
	r := request{URL: req.URL.String(), Method: req.Method, Header: req.Header.Clone()}
	if req.Body != nil {
		data, _ := io.ReadAll(req.Body)
		req.Body.Close()
		if len(data) > 0 {
			if err := json.Unmarshal(data, &r.Body); err != nil {
				return nil, err
			}
		}
	}
	s.mu.Lock()
	s.calls = append(s.calls, r)
	handler := s.handler
	s.mu.Unlock()
	out := reply{status: 200}
	switch v := handler(r).(type) {
	case reply:
		out = v
	case string:
		out.body = []byte(v)
	case []byte:
		out.body = v
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		out.body = data
	}
	return &http.Response{
		StatusCode: out.status, Status: fmt.Sprintf("%d %s", out.status, http.StatusText(out.status)),
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{},
		Body: io.NopCloser(bytes.NewReader(out.body)), ContentLength: int64(len(out.body)), Request: req,
	}, nil
}

// Calls is the requests so far.
func (s *stub) Calls() []request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]request(nil), s.calls...)
}

// Forget starts a new record of requests.
func (s *stub) Forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = nil
}

// Posts is the POST requests so far.
func (s *stub) Posts() []request {
	var out []request
	for _, c := range s.Calls() {
		if c.Method == http.MethodPost {
			out = append(out, c)
		}
	}
	return out
}

// serve answers every request with handler until the test ends.
func serve(t *testing.T, handler func(request) any) *stub {
	t.Helper()
	s := &stub{handler: handler}
	fetch.SetTransport(s)
	t.Cleanup(func() { fetch.SetTransport(nil) })
	return s
}

var issued atomic.Int64

// swData is sw.js_data carrying a new visitor id each time: V1, V2, ...
func swData() string {
	inner := make(A, 14)
	inner[13] = fmt.Sprintf("V%d", issued.Add(1))
	data, _ := json.Marshal(A{A{nil, nil, A{A{inner}}}})
	return ")]}'\n" + string(data)
}

func lastVisitor() string { return fmt.Sprintf("V%d", issued.Load()) }

func isSwData(r request) bool { return strings.HasSuffix(r.URL, "/sw.js_data") }

// dig follows keys through decoded JSON objects; nil when one is missing.
func dig(v any, keys ...string) any {
	for _, key := range keys {
		m, ok := v.(M)
		if !ok {
			return nil
		}
		v = m[key]
	}
	return v
}

// expectCode is err as an Error with this code.
func expectCode(t *testing.T, err error, code string) *rustle.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got no error", code)
	}
	e, ok := rustle.AsError(err)
	if !ok {
		t.Fatalf("expected %s, got %T %v", code, err, err)
	}
	if e.Code != code {
		t.Fatalf("expected %s, got %s: %s", code, e.Code, e.Message)
	}
	return e
}

func equal(t *testing.T, got, want any, what string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n got %#v\nwant %#v", what, got, want)
	}
}
