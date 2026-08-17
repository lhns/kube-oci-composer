// Package buildcontroller reconciles DockerBuild objects by running a Job per build.
//
// A separate package from internal/controller, not extra files in it. The two kinds make different
// promises — one is a pure function of its spec, the other records a hash of its inputs — and
// sharing a package would let a refactor of the composer's reconcile silently change the builder's.
// ADR 0004 wanted separate deployments for the same reason applied to RBAC; this is the same
// argument applied to code.
package buildcontroller

import (
	"fmt"
	"time"
)

// The composer's error triage, re-typed rather than shared.
//
// The shapes are identical because the conditions are: terminal maps to Stalled, pending to
// Reconciling with a short fixed retry. What differs is WHICH failures qualify, and that
// difference is the reason these are not imported from internal/controller — see the note on
// terminal below.

// terminalError marks a failure that retrying cannot fix.
//
// The bar is the same as the composer's and it excludes far more here: editing THIS object's spec
// must be what fixes it. A failing RUN does not qualify, because the Dockerfile that would fix it
// lives inside a Flux source — a different object, whose change raises no generation bump here, so
// stalling would wait for an event that never arrives. In practice only genuinely spec-level
// mistakes stall: an unparsable platform, a malformed resource quantity, a push target that cannot
// be parsed. Build failures take the capped backoff in status.failures instead.
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
