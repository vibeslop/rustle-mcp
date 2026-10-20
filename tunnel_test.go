package main

import "testing"

func TestIPv4Only(t *testing.T) {
	profile := "[Interface]\nPrivateKey = abc=\nAddress = 172.16.0.2/32, 2606:4700:110::1/128\nDNS = 1.1.1.1, 2606:4700:4700::1111\n[Peer]\nPublicKey = xyz=\nAllowedIPs = 0.0.0.0/0, ::/0\nEndpoint = engage.cloudflareclient.com:2408\n"
	want := "[Interface]\nPrivateKey = abc=\nAddress = 172.16.0.2/32\nDNS = 1.1.1.1\n[Peer]\nPublicKey = xyz=\nAllowedIPs = 0.0.0.0/0\nEndpoint = engage.cloudflareclient.com:2408\n"
	if got := ipv4Only(profile); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}
