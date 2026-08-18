// Package buildcontroller reconciles DockerBuild objects by running a Job per build.
//
// A separate package from internal/controller: separate promises, separate RBAC (ADR 0004, 0025).
package buildcontroller

import (
	"fmt"
	"time"
)

// The composer's error triage, re-typed rather than shared. The shapes are identical; what differs
// is WHICH failures qualify, which is the reason they are not imported — see terminal below.

// terminalError marks a failure that retrying cannot fix.
//
// The bar is ADR 0009's and it excludes far more here: editing THIS object's spec must be what
// fixes it. A failing RUN does not qualify — the Dockerfile lives in a Flux source, whose change
// raises no generation bump here, so stalling would wait for an event that never arrives. Only
// spec-level mistakes stall: an unparsable platform, a malformed quantity, an unparsable push
// target. Build failures take the capped backoff in status.failures instead.
type terminalError struct{ err error }

func (t *terminalError) Error() string { return t.err.Error() }
func (t *terminalError) Unwrap() error { return t.err }

func terminal(format string, a ...any) error {
	return &terminalError{err: fmt.Errorf(format, a...)}
}

// pendingError marks a dependency that is absent or not ready yet — the Flux source holding the
// context, a Secret, the builder image.
type pendingError struct{ err error }

func (p *pendingError) Error() string { return p.err.Error() }
func (p *pendingError) Unwrap() error { return p.err }

func pending(format string, a ...any) error {
	return &pendingError{err: fmt.Errorf(format, a...)}
}

const (
	// pendingRetryInterval matches the composer's, for the same reason: a same-commit apply
	// converges without anyone noticing, and a genuinely missing reference costs a couple of cheap
	// GETs a minute rather than a hot loop.
	pendingRetryInterval = 30 * time.Second

	// buildPollInterval is how often a running Job is re-observed. A build takes minutes, so
	// polling faster buys nothing; the Job is also watched, so this is a backstop rather than the
	// primary signal.
	buildPollInterval = 15 * time.Second

	// maxFailureBackoff caps the retry interval after repeated failures.
	//
	// A ceiling rather than unbounded exponential backoff, because a build that fails is usually
	// waiting for a human to push a fix to a Dockerfile — and the retry is what notices the fix,
	// since a push to the source moves the input hash but a controller backing off for hours would
	// not act on it promptly.
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
