package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// tunnel puts a WireGuard tunnel in front of a hosted server's requests when wireproxy is
// installed, as it is in the container image: YouTube refuses most hosting providers'
// addresses, but a VPN's can pass. The profile is WIREGUARD_CONFIG when set (any VPN's,
// or a saved WARP one), else a free Cloudflare WARP device registered now; RUSTLE_WARP=0
// goes without WARP. It returns the routes to try in order, and wireproxy to stop at exit.
func tunnel() (routes []string, wireproxy *exec.Cmd) {
	binary, err := exec.LookPath("wireproxy")
	profile := os.Getenv("WIREGUARD_CONFIG")
	if err != nil || profile == "" && os.Getenv("RUSTLE_WARP") == "0" {
		return nil, nil
	}
	if profile == "" {
		// A cold start waits for this, so it gets a few seconds, not a retry.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		profile, err = registerWARP(ctx)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "rustle: starting without a tunnel, so YouTube will likely refuse this server: %v\n", err)
			return nil, nil
		}
	}
	dir, err := os.MkdirTemp("", "rustle-tunnel-")
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, "wireguard.conf"), []byte(ipv4Only(profile)), 0o600)
	}
	if err == nil {
		config := "WGConfig = " + filepath.Join(dir, "wireguard.conf") + "\n\n[http]\nBindAddress = 127.0.0.1:1080\n"
		err = os.WriteFile(filepath.Join(dir, "wireproxy.conf"), []byte(config), 0o600)
	}
	cmd := exec.Command(binary, "-s", "-c", filepath.Join(dir, "wireproxy.conf"), "-i", "127.0.0.1:1081")
	cmd.Stderr = os.Stderr
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "rustle: starting without a tunnel: %v\n", err)
		return nil, nil
	}
	// The first request, YouTube's visitor id, goes out as soon as the server starts.
	for range 20 {
		if r, err := http.Get("http://127.0.0.1:1081/metrics"); err == nil {
			r.Body.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// After the tunnel: a proxy set on the host (a paid one, say, used only while YouTube
	// refuses the tunnel), then none.
	routes = []string{"http://127.0.0.1:1080"}
	if proxy := os.Getenv("HTTPS_PROXY"); proxy != "" {
		routes = append(routes, proxy)
	}
	return append(routes, "direct"), cmd
}

var profileList = regexp.MustCompile(`(?im)^(\s*(?:Address|AllowedIPs|DNS)\s*=\s*)(.*)$`)

// ipv4Only keeps a WireGuard profile's IPv4 addresses. From Vercel, YouTube refused WARP's
// IPv6 exits five times in six; over IPv4 alone, it refused none of about 90 calls.
func ipv4Only(profile string) string {
	return profileList.ReplaceAllStringFunc(profile, func(line string) string {
		m := profileList.FindStringSubmatch(line)
		var kept []string
		for _, item := range strings.Split(m[2], ",") {
			if item = strings.TrimSpace(item); item != "" && !strings.Contains(item, ":") {
				kept = append(kept, item)
			}
		}
		return m[1] + strings.Join(kept, ", ")
	})
}
