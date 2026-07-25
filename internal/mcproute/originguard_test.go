package mcproute

import "testing"

func TestAllowedHost_Loopback(t *testing.T) {
	cases := []struct {
		host string
		port int
		want bool
	}{
		{"127.0.0.1:9200", 9200, true},
		{"localhost:9200", 9200, true},
		{"127.0.0.1:9200", 9201, false},
		{"evil.example:9200", 9200, false},
		{"localhost", 80, true},
		{"127.0.0.1", 80, true},
		{"localhost:81", 80, false},
		{"localhost", 9200, false}, // bare host only accepted on default port 80
		{"127.0.0.1:9200", 0, false},
	}
	for _, c := range cases {
		if got := AllowedHost(c.host, c.port); got != c.want {
			t.Errorf("AllowedHost(%q, %d) = %v, want %v", c.host, c.port, got, c.want)
		}
	}
}

func TestAllowedOrigin_Loopback(t *testing.T) {
	cases := []struct {
		origin string
		port   int
		want   bool
	}{
		{"http://127.0.0.1:9200", 9200, true},
		{"http://localhost:9200", 9200, true},
		{"http://127.0.0.1:9200", 9201, false},
		{"http://evil.example.com", 9200, false},
		{"https://127.0.0.1:9200", 9200, false},          // wrong scheme
		{"http://user@127.0.0.1:9200", 9200, false},      // userinfo present
		{"http://127.0.0.1:9200/path", 9200, false},      // non-root path
		{"http://127.0.0.1:9200?q=1", 9200, false},        // query string
		{"not a url", 9200, false},
	}
	for _, c := range cases {
		if got := AllowedOrigin(c.origin, c.port); got != c.want {
			t.Errorf("AllowedOrigin(%q, %d) = %v, want %v", c.origin, c.port, got, c.want)
		}
	}
}

// TestAllowedHost_PortIndependence is the claim-4-at-unit-level regression: a
// second, independently-bound port (the front daemon's port Q) must judge its
// OWN loopback origin correctly and independently of any other port (the
// GUI's port P) — the whole reason this guard needed to become port-bound
// rather than reading a single shared Server's port.
func TestAllowedHost_PortIndependence(t *testing.T) {
	const portP = 9125
	const portQ = 9200
	if !AllowedHost("127.0.0.1:9200", portQ) {
		t.Fatal("route daemon's own port must accept its own loopback host")
	}
	if AllowedHost("127.0.0.1:9200", portP) {
		t.Fatal("GUI's port guard must NOT accept the route daemon's port")
	}
	if AllowedHost("127.0.0.1:9125", portQ) {
		t.Fatal("route daemon's port guard must NOT accept the GUI's port")
	}
}
