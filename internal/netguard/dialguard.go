package netguard

import (
	"context"
	"fmt"
	"net"
	"syscall"
	"time"
)

// Threat-model gap I6: SSRF via a spec's fetch.url. Digest verification limits what comes back,
// not where the request goes.
//
//   - Link-local (169.254.0.0/16, fe80::/10, including the cloud metadata endpoint) is ALWAYS
//     blocked.
//   - Other private ranges only with --fetch-deny-private: an in-cluster artifact server on a
//     private address is the ordinary deployment.
//
// Enforced in the dialer, not by parsing the URL, so DNS names, redirects and DNS rebinding are
// all covered.

// DialGuard refuses connections to addresses a layer source has no business being at.
type DialGuard struct {
	// DenyPrivate additionally refuses RFC1918, loopback, CGNAT and unique-local addresses.
	DenyPrivate bool
}

// ErrBlockedAddress is returned when a fetch is refused for its destination, so the caller can
// report why rather than a bare connection error.
type ErrBlockedAddress struct {
	Host   string
	IP     string
	Reason string
}

func (e *ErrBlockedAddress) Error() string {
	return fmt.Sprintf("refusing to fetch from %s (%s): %s", e.Host, e.IP, e.Reason)
}

// blocked reports why an address is refused, or "" if it is allowed.
func (g DialGuard) blocked(ip net.IP) string {
	switch {
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return "link-local addresses host cloud metadata endpoints, which hand out credentials"
	case ip.IsUnspecified():
		return "the unspecified address is not a layer source"
	case !g.DenyPrivate:
		return ""
	case ip.IsLoopback():
		return "loopback reaches the controller's own process (--fetch-deny-private)"
	case ip.IsPrivate():
		return "private addresses are refused by --fetch-deny-private"
	case ip.To4() != nil && ip[len(ip)-4] == 100 && ip[len(ip)-3]&0xc0 == 64:
		// 100.64.0.0/10, CGNAT, which IsPrivate misses; some managed Kubernetes providers use it.
		return "carrier-grade NAT addresses are refused by --fetch-deny-private"
	default:
		return ""
	}
}

// DialContext is an http.Transport.DialContext that applies the guard.
//
// The check runs in Control, after resolution and immediately before connect(2), leaving no window
// for a DNS rebind.
func (g DialGuard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			h, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(h)
			if ip == nil {
				return fmt.Errorf("unparseable address %q", address)
			}
			if reason := g.blocked(ip); reason != "" {
				return &ErrBlockedAddress{Host: host, IP: ip.String(), Reason: reason}
			}
			return nil
		},
	}
	return d.DialContext(ctx, network, addr)
}
