package management

import (
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// The OAuth callback forwarder answers on loopback and on nothing else.
func TestCallbackForwarderListensOnLoopbackOnly(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	fwd, err := startCallbackForwarder(port, "test", "http://127.0.0.1:8317/test/callback")
	if err != nil {
		t.Fatal(err)
	}
	defer stopCallbackForwarderInstance(port, fwd)

	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/callback?code=c&state=s")
	if err != nil {
		t.Fatalf("loopback: %v", err)
	}
	_ = resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || loc != "http://127.0.0.1:8317/test/callback?code=c&state=s" {
		t.Fatalf("loopback: got %d %q", resp.StatusCode, loc)
	}

	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.To4() == nil {
			continue
		}
		target := net.JoinHostPort(ipNet.IP.String(), strconv.Itoa(port))
		if conn, errDial := net.DialTimeout("tcp", target, time.Second); errDial == nil {
			_ = conn.Close()
			t.Fatalf("forwarder answered on %s", target)
		}
		return
	}
	t.Log("no non-loopback IPv4 address; only the loopback half ran")
}
