package api

import (
	"net/http/httptest"
	"testing"
)

func TestDashboardRequestAllowed(t *testing.T) {
	const serve = "jacks-desktop.example.ts.net"
	tailnet := []string{serve, "jacks-desktop"}
	cases := []struct {
		host, origin string
		peer, login  string
		hosts        []string
		want         bool
	}{
		{host: "127.0.0.1:8317", want: true},
		{host: "localhost:8317", origin: "http://localhost:8317", want: true},
		{host: "[::1]:8317", want: true},
		{host: "127.0.0.1:8317", origin: "https://evil.example", want: false},  // another website reading the data
		{host: "evil.example:8317", want: false},                               // DNS rebinding
		{host: "127.0.0.1:8317", origin: "http://localhost:8317", want: false}, // origin must match host exactly

		// Tailscale Serve: loopback peer, tailnet login header, listed host.
		{host: serve, origin: "http://" + serve, peer: "127.0.0.1:50000", login: "jack@example.com", hosts: tailnet, want: true},
		{host: "JACKS-DESKTOP", peer: "[::1]:50000", login: "jack@example.com", hosts: tailnet, want: true},
		{host: serve, peer: "127.0.0.1:50000", hosts: tailnet, want: false},                                                           // no Serve header
		{host: serve, peer: "100.64.0.7:50000", login: "jack@example.com", hosts: tailnet, want: false},                               // not via Serve
		{host: "evil.example", peer: "127.0.0.1:50000", login: "jack@example.com", hosts: tailnet, want: false},                       // DNS rebinding
		{host: serve, origin: "http://evil.example", peer: "127.0.0.1:50000", login: "jack@example.com", hosts: tailnet, want: false}, // cross-origin
		{host: serve, peer: "127.0.0.1:50000", login: "jack@example.com", want: false},                                                // empty list: loopback only
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "/dashboard/data", nil)
		r.Host = tc.host
		if tc.peer != "" {
			r.RemoteAddr = tc.peer
		}
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if tc.login != "" {
			r.Header.Set("Tailscale-User-Login", tc.login)
		}
		r.Header.Set("X-Forwarded-For", "127.0.0.1") // never trusted
		if got := dashboardRequestAllowed(r, tc.hosts); got != tc.want {
			t.Errorf("host=%q origin=%q peer=%q login=%q hosts=%v: got %v, want %v", tc.host, tc.origin, tc.peer, tc.login, tc.hosts, got, tc.want)
		}
	}
}
