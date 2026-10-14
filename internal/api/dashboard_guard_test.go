package api

import (
	"net/http/httptest"
	"testing"
)

func TestDashboardRequestAllowed(t *testing.T) {
	cases := []struct {
		host, origin string
		want         bool
	}{
		{"127.0.0.1:8317", "", true},
		{"localhost:8317", "http://localhost:8317", true},
		{"[::1]:8317", "", true},
		{"127.0.0.1:8317", "https://evil.example", false},  // another website reading the data
		{"evil.example:8317", "", false},                   // DNS rebinding
		{"127.0.0.1:8317", "http://localhost:8317", false}, // origin must match host exactly
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "/dashboard/data", nil)
		r.Host = tc.host
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if got := dashboardRequestAllowed(r); got != tc.want {
			t.Errorf("host=%q origin=%q: got %v, want %v", tc.host, tc.origin, got, tc.want)
		}
	}
}
