package oci

import (
	"context"
	"fmt"
	"net"
	"syscall"
	"time"
)

// Threat-model gap I6: an SSRF via a `fetch` URL.
//
// `fetch.url` comes from a spec, so anyone with `create` on an ImageComposition chooses where the
// controller sends a GET, from the controller's network position. Digest verification limits what
// can come BACK -- a response that does not match the declared digest never becomes a layer -- but
// it does nothing about the request itself, and a blind GET is enough to reach a cloud metadata
// endpoint or an internal service that acts on one.
//
// The obvious mitigation, blocking every private range, would break the project's most ordinary
// deployment: an artifact server on a private address inside the same cluster. Fetching from RFC1918
// is not a smell here, it is the normal case. So the two are separated by how defensible each is:
//
//   - Link-local (169.254.0.0/16, fe80::/10) is ALWAYS blocked. 169.254.169.254 is the cloud
//     metadata endpoint on AWS, GCP, Azure and Hetzner alike, and it hands out credentials to
//     anything that asks. No legitimate layer source lives there.
//   - Everything else private is blocked only when the operator asks for it, because on many
//     clusters that would refuse the sources people actually use.
//
// Enforced in a DialContext rather than by parsing the URL, which is what makes it hold: a host
// name resolving to 169.254.169.254, a redirect to it, and a DNS rebind that answers differently
// the second time all arrive at the dialer, and none of them can be seen by inspecting the string.

// DialGuard refuses connections to addresses a layer source has no business being at.
type DialGuard struct {
	// DenyPrivate additionally refuses RFC1918, loopback, CGNAT and unique-local addresses.
	// Off by default: see above.
	DenyPrivate bool
}

// ErrBlockedAddress is returned when a fetch is refused for its destination rather than its
// content. Distinct so the caller can report *why* clearly -- "connection refused" for a blocked
// metadata endpoint is the kind of message that sends someone debugging their network for an hour.
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
		// 100.64.0.0/10, CGNAT. net has no IsPrivate for it, and it is where several managed
		// Kubernetes providers put node and service networks.
		return "carrier-grade NAT addresses are refused by --fetch-deny-private"
	default:
		return ""
	}
}

// DialContext is an http.Transport.DialContext that applies the guard.
//
// The check runs in Control, after the address is resolved and immediately before connect(2), so
// there is no window between deciding an address is safe and using it -- which is precisely the
// window a DNS rebind needs.
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
