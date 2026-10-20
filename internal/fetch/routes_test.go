package fetch

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func useRoutes(t *testing.T, proxies ...string) {
	t.Helper()
	if err := SetRoutes(proxies); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { SetRoutes(nil) })
}

func TestRoutesMoveDownWhileRefused(t *testing.T) {
	if err := SetRoutes([]string{"ftp://example.com"}); err == nil {
		t.Error("ftp:// accepted as a proxy")
	}
	useRoutes(t, "http://127.0.0.1:1080", " socks5://127.0.0.1:1081", "direct")
	if Route() != 0 {
		t.Fatalf("route %d at first", Route())
	}
	if !Refuse(0) || Route() != 1 {
		t.Fatalf("route %d after refusing the first", Route())
	}
	if !Refuse(1) || Route() != 2 {
		t.Fatalf("route %d after refusing the second", Route())
	}
	// Every route refused: back to the preferred one, and nothing left to switch to.
	if Refuse(2) || Route() != 0 {
		t.Fatalf("route %d with every route refused", Route())
	}
	// After a rest, the preferred route gets another chance; the others still rest.
	routes.Lock()
	routes.list[0].refused = time.Now().Add(-routeRest)
	routes.Unlock()
	if Route() != 0 || Refuse(0) {
		t.Fatalf("route %d after the first rested", Route())
	}
}

func TestRoutesPickTheProxy(t *testing.T) {
	useRoutes(t, "http://127.0.0.1:1080", "direct")
	away, _ := http.NewRequest("GET", "https://www.youtube.com/", nil)
	if p, _ := proxy(away); p == nil || p.Host != "127.0.0.1:1080" {
		t.Errorf("proxy %v for YouTube", p)
	}
	here, _ := http.NewRequest("GET", "http://localhost:8080/", nil)
	if p, _ := proxy(here); p != nil {
		t.Errorf("proxy %v for this machine", p)
	}
	Refuse(0)
	if p, _ := proxy(away); p != nil {
		t.Errorf("proxy %v on the direct route", p)
	}
}

// A tunnel that died is skipped: the retry goes out through the next proxy.
func TestRoutesSkipAProxyThatIsDown(t *testing.T) {
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	deadAddr := dead.Addr().String()
	dead.Close()
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A proxy is asked for the whole URL.
		if r.URL.Host != "example.invalid" {
			http.Error(w, "not proxied", http.StatusBadGateway)
			return
		}
		w.Write([]byte("via the second proxy"))
	}))
	defer alive.Close()
	SetTransport(nil)
	useRoutes(t, "http://"+deadAddr, alive.URL)
	r, err := Get(context.Background(), "http://example.invalid/clip", Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Body) != "via the second proxy" || Route() != 1 {
		t.Errorf("body %q, route %d", r.Body, Route())
	}
}
