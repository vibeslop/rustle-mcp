package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

type roundTrip func(req *http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// use answers every request with rt for the rest of the test.
func use(t *testing.T, rt roundTrip) {
	t.Cleanup(func() { SetTransport(nil) })
	SetTransport(rt)
}

func answer(req *http.Request, status int, body io.Reader) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(body), ContentLength: -1, Request: req}
}

// calls records each request's context, to tell whether it was cancelled.
type calls struct {
	mu   sync.Mutex
	ctxs []context.Context
}

func (c *calls) add(ctx context.Context) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ctxs = append(c.ctxs, ctx)
	return len(c.ctxs) - 1
}

func (c *calls) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ctxs)
}

func (c *calls) cancelled(n int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ctxs[n].Err() != nil
}

// scripted is a transport whose nth call answers after delays[n], or fails when delays[n]
// is negative.
func scripted(t *testing.T, delays ...time.Duration) *calls {
	made := &calls{}
	use(t, func(req *http.Request) (*http.Response, error) {
		n := made.add(req.Context())
		delay := delays[n]
		timer := time.NewTimer(max(delay, -delay))
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		if delay < 0 {
			return nil, errors.New("fetch failed")
		}
		return answer(req, 200, strings.NewReader(fmt.Sprintf("answer %d", n))), nil
	})
	return made
}

func TestAStalledRequestIsSentAgainAndTheFirstAnswerWins(t *testing.T) {
	made := scripted(t, time.Second, 10*time.Millisecond)
	started := time.Now()
	result, err := Get(context.Background(), "https://example.com/", Options{Hedge: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Body) != "answer 1" {
		t.Errorf("body %q, want the second copy's", result.Body)
	}
	// Before the stalled copy would have answered; a loaded machine stretches the rest.
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Errorf("took %v", elapsed)
	}
	if made.len() != 2 {
		t.Errorf("%d requests, want 2", made.len())
	}
	if !made.cancelled(0) {
		t.Error("the slower copy is not cancelled")
	}
}

func TestAQuickAnswerSendsNoSecondCopy(t *testing.T) {
	made := scripted(t, 5*time.Millisecond, 5*time.Millisecond)
	result, err := Get(context.Background(), "https://example.com/", Options{Hedge: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Body) != "answer 0" {
		t.Errorf("body %q, want the first copy's", result.Body)
	}
	time.Sleep(250 * time.Millisecond)
	if made.len() != 1 {
		t.Errorf("%d requests, want 1", made.len())
	}
}

func TestACopyThatFailsOutrightIsReplacedAtOnce(t *testing.T) {
	made := scripted(t, -5*time.Millisecond, 5*time.Millisecond)
	result, err := Get(context.Background(), "https://example.com/", Options{Hedge: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Body) != "answer 1" {
		t.Errorf("body %q, want the second copy's", result.Body)
	}
	if made.len() != 2 {
		t.Errorf("%d requests, want 2", made.len())
	}
}

// stalling is a body that yields its head, then (when stall) waits until its request is
// cancelled or a second has passed before its end.
type stalling struct {
	ctx   context.Context
	parts []string
	stall bool
}

func (s *stalling) Read(p []byte) (int, error) {
	if len(s.parts) == 0 {
		return 0, io.EOF
	}
	if len(s.parts) == 1 && s.stall {
		s.stall = false
		select {
		case <-s.ctx.Done():
			return 0, s.ctx.Err()
		case <-time.After(time.Second):
		}
	}
	n := copy(p, s.parts[0])
	s.parts = s.parts[1:]
	return n, nil
}

func TestABodyThatStallsAfterItsHeadersIsHedgedToo(t *testing.T) {
	made := &calls{}
	use(t, func(req *http.Request) (*http.Response, error) {
		n := made.add(req.Context())
		body := &stalling{ctx: req.Context(), parts: []string{fmt.Sprintf("part %d ", n), "end"}, stall: n == 0}
		return answer(req, 200, body), nil
	})
	started := time.Now()
	result, err := Get(context.Background(), "https://example.com/", Options{Hedge: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Body) != "part 1 end" {
		t.Errorf("body %q, want the second copy's", result.Body)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Errorf("took %v", elapsed)
	}
}

func TestAnHTTPErrorIsAnAnswerNotAReasonToHedge(t *testing.T) {
	made := &calls{}
	use(t, func(req *http.Request) (*http.Response, error) {
		made.add(req.Context())
		return answer(req, 404, strings.NewReader("gone")), nil
	})
	_, err := Get(context.Background(), "https://example.com/", Options{Hedge: 30 * time.Millisecond})
	if !rustle.HasCode(err, "SOURCE_NOT_FOUND") {
		t.Errorf("error %v, want SOURCE_NOT_FOUND", err)
	}
	if made.len() != 1 {
		t.Errorf("%d requests, want 1", made.len())
	}
}
