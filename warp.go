package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/vibeslop/rustle-mcp/internal/fetch"
)

// Cloudflare's WARP app registers devices here; wgcf tracks the version the app sends.
const (
	warpAPI     = "https://api.cloudflareclient.com/v0a5641/reg"
	warpVersion = "a-6.38.9-5641"
)

// warp registers a free Cloudflare WARP device and prints its WireGuard profile. A hosted
// server sends its requests through it (scripts/hosted.sh): YouTube refuses most hosting
// providers' addresses, and WARP's pass.
func warp() int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	profile, err := registerWARP(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rustle: registering with Cloudflare WARP failed: %v\n", err)
		return 1
	}
	fmt.Print(profile)
	return 0
}

func registerWARP(ctx context.Context) (string, error) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]any{
		"key": base64.StdEncoding.EncodeToString(key.PublicKey().Bytes()), "tos": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"model": "rustle-mcp", "locale": "en_US", "key_type": "curve25519", "tunnel_type": "wireguard",
		"fcm_token": "", "install_id": "", "serial_number": "",
	})
	response, err := fetch.Get(ctx, warpAPI, fetch.Options{Method: "POST", Body: body, What: "Cloudflare", Limit: 1 << 20, Timeout: 10 * time.Second,
		Headers: map[string]string{"Content-Type": "application/json; charset=UTF-8", "User-Agent": "1.1.1.1/6.38.9-5641 (Android 16.0.0)", "CF-Client-Version": warpVersion}})
	if err != nil {
		return "", err
	}
	var device struct {
		Config struct {
			Peers []struct {
				PublicKey string `json:"public_key"`
				Endpoint  struct {
					Host string `json:"host"`
				} `json:"endpoint"`
			} `json:"peers"`
			Interface struct {
				Addresses struct {
					V4 string `json:"v4"`
					V6 string `json:"v6"`
				} `json:"addresses"`
			} `json:"interface"`
		} `json:"config"`
	}
	if json.Unmarshal(response.Body, &device) != nil || len(device.Config.Peers) == 0 || device.Config.Interface.Addresses.V4 == "" {
		return "", fmt.Errorf("Cloudflare's answer has no WireGuard peer")
	}
	peer := device.Config.Peers[0]
	endpoint := peer.Endpoint.Host
	if _, _, err := net.SplitHostPort(endpoint); err != nil {
		endpoint = "engage.cloudflareclient.com:2408"
	}
	address := device.Config.Interface.Addresses.V4 + "/32"
	if v6 := device.Config.Interface.Addresses.V6; v6 != "" {
		address += ", " + v6 + "/128"
	}
	return fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = %s
DNS = 1.1.1.1, 1.0.0.1
MTU = 1280
[Peer]
PublicKey = %s
AllowedIPs = 0.0.0.0/0, ::/0
Endpoint = %s
`, base64.StdEncoding.EncodeToString(key.Bytes()), address, peer.PublicKey, endpoint), nil
}
