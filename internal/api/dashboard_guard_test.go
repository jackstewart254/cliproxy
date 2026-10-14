package api

import (
	"net/http/httptest"
	"testing"
)

func TestDashboardRequestAllowed(t *testing.T) {
	const (
		loopback = "127.0.0.1:50000"
		tailnet  = "100.101.102.103:50000"
		public   = "203.0.113.9:50000"
	)
	cases := []struct {
		peer, host, origin string
		want               bool
	}{
		{loopback, "127.0.0.1:8317", "", true},
		{loopback, "localhost:8317", "http://localhost:8317", true},
		{loopback, "[::1]:8317", "", true},
		{loopback, "127.0.0.1:8317", "https://evil.example", false},  // another website reading the data
		{loopback, "evil.example:8317", "", false},                   // DNS rebinding
		{loopback, "127.0.0.1:8317", "http://localhost:8317", false}, // origin must match host exactly
		// The tailnet dashboard and sign-in helper, by MagicDNS name or tailnet IP.
		{tailnet, "jacks-mac.tail1234.ts.net:8317", "", true},
		{tailnet, "jacks-mac.tail1234.ts.net:8317", "http://jacks-mac.tail1234.ts.net:8317", true},
		{tailnet, "100.64.0.7:8317", "", true},
		{"[fd7a:115c:a1e0::2]:50000", "[fd7a:115c:a1e0::1]:8317", "", true},
		{loopback, "jacks-mac.tail1234.ts.net", "", true}, // tailscale serve proxying from loopback
		{tailnet, "evil.example:8317", "", false},
		{tailnet, "100.64.0.7:8317", "https://evil.example", false},
		// A public caller is turned away whatever Host it sends.
		{public, "jacks-mac.tail1234.ts.net:8317", "", false},
		{public, "100.64.0.7:8317", "", false},
		{public, "127.0.0.1:8317", "", false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "/dashboard/data", nil)
		r.RemoteAddr = tc.peer
		r.Host = tc.host
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if got := dashboardRequestAllowed(r); got != tc.want {
			t.Errorf("peer=%q host=%q origin=%q: got %v, want %v", tc.peer, tc.host, tc.origin, got, tc.want)
		}
	}
}
