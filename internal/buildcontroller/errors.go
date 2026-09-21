// Package buildcontroller reconciles ImageBuild objects by running a Job per build.
//
// A separate package from internal/controller: separate promises, separate RBAC (ADR 0004, 0025).
package buildcontroller

import (
	"time"
)

const (
	// pendingRetryInterval matches the composer's, for the same reason: a same-commit apply
	// converges without anyone noticing, and a genuinely missing reference costs a couple of cheap
	// GETs a minute rather than a hot loop.
	pendingRetryInterval = 30 * time.Second

	// defaultBuildPollInterval is how often a running Job is re-observed. A build takes minutes,
	// so polling faster buys nothing; the Job is also watched, so this is a backstop rather than
	// the primary signal.
	//
	// It is configurable because it bounds something else: the image is pushed UNTAGGED and named
	// when the controller next looks (ADR 0054), so this is how long a build's own output can be
	// reclaimed by a registry's collector. The chart derives gcDelay from it, and a test that wants
	// short retention timings shortens this to shorten that.
	defaultBuildPollInterval = 15 * time.Second

	// maxFailureBackoff caps the retry interval. A ceiling rather than unbounded exponential
	// backoff: a failing build is usually waiting for a human to push a Dockerfile fix, and the
	// retry is what notices it, so backing off for hours would not act on the fix promptly.
	maxFailureBackoff = 10 * time.Minute
)

// failureBackoff returns how long to wait after n consecutive failures.
func failureBackoff(n int32) time.Duration {
	d := pendingRetryInterval
	for range n {
		if d >= maxFailureBackoff {
			break
		}
		d *= 2
	}
	return min(d, maxFailureBackoff)
}
