package serve

import (
	"net"
	"net/http"
)

// loopbackWritesOnly refuses every write to the endpoint that did not come from this process.
//
// The package doc has always said writes arrive only over loopback, and `Handler` said the chart
// binds the write path to localhost. Neither was true: `--serving-bind-address` defaults to ":5000",
// which is every interface, the chart exposes that as a ClusterIP Service, and the Ingress routes
// `/v2/` — a prefix that includes PUT, POST, PATCH and DELETE. There is no authentication anywhere
// in this package. So any pod in the cluster could push a manifest, or repoint a mutable tag at
// content of its choosing, and the only thing standing in the way was that nobody had tried.
//
// ADR 0025 rests on the same false premise when it says a build Job "cannot write to the
// controller's loopback-only serving endpoint". It could.
//
// Reads stay anonymous. That is deliberate and documented (threat model I5): this endpoint exists so
// that a kubelet can pull without credentials, and restricting WHO may pull is a NetworkPolicy
// question rather than this handler's.
func loopbackWritesOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isWrite(r.Method) || fromLoopback(r.RemoteAddr) {
			next.ServeHTTP(w, r)
			return
		}

		// The distribution spec's error shape, so a client reports something it understands rather
		// than a bare 405. DENIED is what a registry returns when the operation is understood and
		// refused, which is exactly the case here.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":[{"code":"DENIED","message":` +
			`"this endpoint accepts writes only from the controller itself, over loopback"}]}` + "\n"))
	})
}

func isWrite(method string) bool {
	switch method {
	case http.MethodPut, http.MethodPost, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// fromLoopback reports whether the peer is on this machine.
//
// RemoteAddr is the real TCP peer, not a header, so it cannot be forged by a client — and anything
// that terminates the connection first (an Ingress, a Service proxy) appears as its own address and
// is refused, which is the intended answer. A request that genuinely reaches loopback is either this
// process or something already inside its network namespace, which has this pod anyway.
func fromLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// No port means this is not a TCP peer this code understands. Refuse rather than guess:
		// the failure mode of guessing wrong is an open write path.
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
