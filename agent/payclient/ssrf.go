package payclient

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// GuardedClient returns an *http.Client whose dialer refuses loopback,
// private, link-local, and unspecified addresses (and the agent's own
// address), so an outbound paid fetch cannot reach the agent's control API or
// other local services. The check runs at dial time against the resolved IP
// and pins that IP for the connection, so DNS rebinding cannot slip past it.
//
// allow holds explicit "host:port" exceptions (for example a local gateway in
// development, DCR402_AGENT_FETCH_ALLOW); self ("host:port", the agent's own
// listen address) is refused even when it would otherwise be reachable.
// timeout bounds each request.
func GuardedClient(self string, allow []string, timeout time.Duration) *http.Client {
	allowSet := make(map[string]bool, len(allow))
	for _, a := range allow {
		if a = strings.TrimSpace(a); a != "" {
			allowSet[a] = true
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if self != "" && addr == self {
			return nil, fmt.Errorf("payclient: refusing to fetch the agent's own address %q", addr)
		}
		allowed := allowSet[addr]
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("payclient: no address for %q", host)
		}
		var target net.IP
		for _, ipa := range ips {
			if !allowed && isInternalIP(ipa.IP) {
				return nil, fmt.Errorf("payclient: refusing internal address %s for %q "+
					"(set DCR402_AGENT_FETCH_ALLOW=%s to override)", ipa.IP, host, addr)
			}
			if target == nil {
				target = ipa.IP
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(target.String(), port))
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

// isInternalIP reports whether ip is in a range an outbound paid fetch must
// never reach: loopback, RFC1918/ULA private, link-local, or unspecified.
func isInternalIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}
