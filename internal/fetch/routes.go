package fetch

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// A route is one way out to the network: a proxy, or none. YouTube trusts some addresses
// more than others, and its trust in one can drop at any time, so a hosted server lists
// several in RUSTLE_PROXIES and moves down the list while YouTube refuses the one in use.
type route struct {
	proxy   *url.URL // nil goes direct
	refused time.Time
}

// routeRest is how long a refused route is skipped. The first route is the preferred
// one, so it gets another chance after a rest.
const routeRest = 10 * time.Minute

var routes struct {
	sync.Mutex
	list []*route
}

// SetRoutes makes requests leave through the first of these proxies YouTube hasn't
// refused lately; "direct" is none. Without routes, HTTPS_PROXY applies as usual.
func SetRoutes(proxies []string) error {
	var list []*route
	for _, p := range proxies {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if p == "direct" {
			list = append(list, &route{})
			continue
		}
		u, err := url.Parse(p)
		if err != nil || u.Host == "" || !(u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "socks5" || u.Scheme == "socks5h") {
			return fmt.Errorf("RUSTLE_PROXIES: %q is not a proxy URL (http://host:port, socks5://host:port) or direct", p)
		}
		list = append(list, &route{proxy: u})
	}
	routes.Lock()
	routes.list = list
	routes.Unlock()
	return nil
}

// current is the index of the route requests take now: the first not refused in the last
// routeRest, or the first when all are; -1 without routes.
func current() int {
	if len(routes.list) == 0 {
		return -1
	}
	for i, r := range routes.list {
		if time.Since(r.refused) >= routeRest {
			return i
		}
	}
	return 0
}

// Route is the route requests take now, for Refuse; -1 without routes.
func Route() int {
	routes.Lock()
	defer routes.Unlock()
	return current()
}

// Refuse marks route i as refused, and is whether requests now take one that isn't.
func Refuse(i int) bool {
	routes.Lock()
	defer routes.Unlock()
	if i < 0 || i >= len(routes.list) {
		return false
	}
	routes.list[i].refused = time.Now()
	next := current()
	if next == i || time.Since(routes.list[next].refused) < routeRest {
		return false
	}
	fmt.Fprintf(os.Stderr, "rustle: route %d of %d refused; using route %d for %d minutes\n", i+1, len(routes.list), next+1, int(routeRest.Minutes()))
	return true
}

// proxy picks the proxy for a request: the current route's, except for this machine.
func proxy(r *http.Request) (*url.URL, error) {
	routes.Lock()
	i := current()
	var chosen *url.URL
	if i >= 0 {
		chosen = routes.list[i].proxy
	}
	routes.Unlock()
	if i < 0 {
		return http.ProxyFromEnvironment(r)
	}
	host := r.URL.Hostname()
	if ip := net.ParseIP(host); host == "localhost" || ip != nil && ip.IsLoopback() {
		return nil, nil
	}
	return chosen, nil
}

// proxyDown is true when the proxy itself couldn't be reached: a tunnel that died.
func proxyDown(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "proxyconnect"
}
