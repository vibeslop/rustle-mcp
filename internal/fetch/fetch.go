// Package fetch makes bounded HTTP requests: a size limit, a deadline, one retry of a
// connection that fails before answering, and hedged copies for requests that stall.
package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

const BrowserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

type Options struct {
	Method  string
	Headers map[string]string
	Body    []byte
	// Limit is the largest body accepted, in bytes; 16 MiB when 0.
	Limit int64
	// Timeout covers the whole request, body included; 20 s when 0.
	Timeout time.Duration
	// What names the server in error messages, e.g. "YouTube".
	What string
	// Allow are statuses returned instead of turned into errors.
	Allow []int
	// Hedge sends the same request again if no answer has finished after this long; the
	// first complete answer wins.
	Hedge time.Duration
}

type Response struct {
	Status int
	Header http.Header
	// URL is the final URL, after redirects.
	URL  string
	Body []byte
}

// Two pools of connections: a hedged copy goes out on a connection of its own, since
// HTTP/2 would otherwise put it on the stalled one.
var (
	mu        sync.RWMutex
	primary   http.RoundTripper = newTransport()
	secondary http.RoundTripper = newTransport()
)

func newTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 16
	t.Proxy = proxy
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return t
}

// SetTransport answers every request with rt, for tests; nil restores the network.
func SetTransport(rt http.RoundTripper) {
	mu.Lock()
	defer mu.Unlock()
	if rt == nil {
		primary, secondary = newTransport(), newTransport()
		return
	}
	primary, secondary = rt, rt
}

// Client is an HTTP client on the primary connections, for requests that read headers only.
func Client() *http.Client {
	mu.RLock()
	defer mu.RUnlock()
	return &http.Client{Transport: primary}
}

func transport(second bool) http.RoundTripper {
	mu.RLock()
	defer mu.RUnlock()
	if second {
		return secondary
	}
	return primary
}

var trace = os.Getenv("RUSTLE_TRACE") == "1"

// Get fetches url. Connections are kept alive per host, so a second request to the same
// host skips the handshake. A connection that fails before any response is retried
// once: YouTube connections occasionally stall and a fresh one answers at once, which is
// cheaper than handing the agent an error it would retry.
func Get(ctx context.Context, url string, o Options) (*Response, error) {
	if !trace {
		return hedged(ctx, url, o)
	}
	started := time.Now()
	r, err := hedged(ctx, url, o)
	method := o.Method
	if method == "" {
		method = "GET"
	}
	shown := url
	if len(shown) > 90 {
		shown = shown[:90]
	}
	fmt.Fprintf(os.Stderr, "rustle trace %dms fetch %s %s\n", time.Since(started).Milliseconds(), method, shown)
	return r, err
}

// hedged sends the same read-only request again, on another connection, if the first
// hasn't finished in time, and the first complete answer wins. YouTube answers most small
// requests in under 100 ms but now and then stalls one, before or during the body, for a
// second or more; the second copy cuts that tail for an occasional duplicate.
func hedged(ctx context.Context, url string, o Options) (*Response, error) {
	if o.Hedge <= 0 {
		return once(ctx, url, o, transport(false))
	}
	type outcome struct {
		r   *Response
		err error
	}
	copies, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan outcome, 2)
	launched, pending := 0, 0
	launch := func() {
		rt := transport(launched > 0)
		launched++
		pending++
		go func() {
			r, err := once(copies, url, o, rt)
			results <- outcome{r, err}
		}()
	}
	launch()
	timer := time.NewTimer(o.Hedge)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			if launched < 2 {
				launch()
			}
		case out := <-results:
			pending--
			if out.err == nil {
				return out.r, nil
			}
			// An HTTP error is an answer; only a failed or stalled transfer gets a second copy.
			if !rustle.HasCode(out.err, "NETWORK_UNAVAILABLE", "NETWORK_TIMEOUT", "CANCELLED") {
				return nil, out.err
			}
			if launched < 2 && ctx.Err() == nil {
				launch()
				continue
			}
			if pending == 0 {
				if ctx.Err() != nil {
					return nil, rustle.Cancelled()
				}
				return nil, out.err
			}
		}
	}
}

func once(ctx context.Context, url string, o Options, rt http.RoundTripper) (*Response, error) {
	limit := o.Limit
	if limit <= 0 {
		limit = 16 << 20
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	method := o.Method
	if method == "" {
		method = http.MethodGet
	}
	client := &http.Client{Transport: rt}
	var lastErr error
	for range 2 {
		attempt, cancel := context.WithTimeout(ctx, timeout)
		var body io.Reader
		if o.Body != nil {
			body = bytes.NewReader(o.Body)
		}
		req, err := http.NewRequestWithContext(attempt, method, url, body)
		if err != nil {
			cancel()
			return nil, rustle.Fail("That URL is malformed.", "INVALID_SOURCE", "")
		}
		req.Header.Set("User-Agent", BrowserUA)
		for k, v := range o.Headers {
			req.Header.Set(k, v)
		}
		via := Route()
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			if ctx.Err() != nil {
				break
			}
			// A proxy that can't be reached is skipped, and the retry takes the next route.
			if proxyDown(err) {
				Refuse(via)
			}
			continue
		}
		if (resp.StatusCode < 200 || resp.StatusCode > 299) && !allowed(o.Allow, resp.StatusCode) {
			resp.Body.Close()
			cancel()
			return nil, rustle.HTTPError(resp.StatusCode, o.What)
		}
		tooLarge := rustle.Fail("The response is larger than allowed.", "DOWNLOAD_TOO_LARGE", "Use a smaller source or a local file.")
		if resp.ContentLength > limit {
			resp.Body.Close()
			cancel()
			return nil, tooLarge
		}
		data, err := readBody(resp.Body, resp.ContentLength, limit)
		resp.Body.Close()
		cancel()
		if err != nil {
			if errors.Is(err, errTooLarge) {
				return nil, tooLarge
			}
			if ctx.Err() != nil {
				return nil, rustle.Cancelled()
			}
			return nil, rustle.NetworkError()
		}
		return &Response{Status: resp.StatusCode, Header: resp.Header, URL: resp.Request.URL.String(), Body: data}, nil
	}
	if ctx.Err() != nil {
		return nil, rustle.Cancelled()
	}
	if errors.Is(lastErr, context.DeadlineExceeded) {
		return nil, rustle.Fail("The network request timed out.", "NETWORK_TIMEOUT", "Wait briefly, then retry.").Retry(0)
	}
	return nil, rustle.NetworkError()
}

var errTooLarge = errors.New("too large")

func readBody(r io.Reader, length, limit int64) ([]byte, error) {
	size := 512
	if length > 0 {
		size = int(length)
	}
	buf := bytes.NewBuffer(make([]byte, 0, size))
	n, err := io.Copy(buf, io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, errTooLarge
	}
	return buf.Bytes(), nil
}

func allowed(statuses []int, status int) bool {
	for _, s := range statuses {
		if s == status {
			return true
		}
	}
	return false
}
