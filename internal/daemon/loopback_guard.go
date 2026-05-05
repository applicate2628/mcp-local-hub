package daemon

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

func rejectUnsafeLoopbackRequest(w http.ResponseWriter, r *http.Request) bool {
	if isSafeLoopbackRequest(r) {
		return false
	}
	http.Error(w, "forbidden loopback request", http.StatusForbidden)
	return true
}

func isSafeLoopbackRequest(r *http.Request) bool {
	if !isLoopbackHost(r.Host) {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" && !isLoopbackOrigin(origin) {
		return false
	}
	return isSafeFetchSite(r.Header.Get("Sec-Fetch-Site"))
}

func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Path != "" && u.Path != "/" {
		return false
	}
	return isLoopbackHost(u.Host)
}

func isSafeFetchSite(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "none", "same-origin", "same-site":
		return true
	default:
		return false
	}
}

func isLoopbackHost(hostport string) bool {
	host, ok := splitHostForLoopbackCheck(hostport)
	if !ok {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

func splitHostForLoopbackCheck(hostport string) (string, bool) {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" || strings.ContainsAny(hostport, "/@") {
		return "", false
	}
	if strings.HasPrefix(hostport, "[") {
		if strings.Contains(hostport, "]:") {
			host, port, err := net.SplitHostPort(hostport)
			if err != nil || !validHostPort(port) {
				return "", false
			}
			return host, true
		}
		if strings.HasSuffix(hostport, "]") {
			return strings.TrimPrefix(strings.TrimSuffix(hostport, "]"), "["), true
		}
		return "", false
	}
	if strings.Count(hostport, ":") == 1 {
		host, port, err := net.SplitHostPort(hostport)
		if err != nil || !validHostPort(port) {
			return "", false
		}
		return host, true
	}
	return hostport, true
}

func validHostPort(port string) bool {
	n, err := strconv.Atoi(port)
	return err == nil && n >= 0 && n <= 65535
}
