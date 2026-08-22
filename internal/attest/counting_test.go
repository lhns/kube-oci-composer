package attest

import (
	"net/http"
	"sync"
)

// countingTransport counts write requests, so a test can assert that a converged pass attaches
// nothing rather than merely asserting that nothing looked different afterwards.
type countingTransport struct {
	inner  http.RoundTripper
	mu     sync.Mutex
	writes int
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.Method {
	case http.MethodPut, http.MethodPost, http.MethodPatch, http.MethodDelete:
		c.mu.Lock()
		c.writes++
		c.mu.Unlock()
	}
	return c.inner.RoundTrip(req)
}
