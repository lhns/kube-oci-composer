// Package buildcontroller reconciles ImageBuild objects by running a Job per build.
//
// A separate package from internal/controller: separate promises, separate RBAC (ADR 0004, 0025).
package buildcontroller

import (
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
	"time"
)

const (
	// defaultBuildPollInterval is how often a running Job is re-observed (the Job is also
	// watched). It does not bound how long a pushed but untagged image waits to be named: that is
	// the Job's whole completion path (ADR 0054). The chart's gcDelay floor is 3x this; a collector
	// faster than naming costs a rebuild (outputGone).
	defaultBuildPollInterval = 15 * time.Second

	// maxFailureBackoff caps the retry interval, so a pushed fix is noticed promptly.
	maxFailureBackoff = 10 * time.Minute
)

// outputLostPrefix starts the message of an attempt whose output the registry no longer has.
// It is also how a later pass recognises that the loss has already been counted.
const outputLostPrefix = "the build's output is gone from the registry"

// outputGoneError is applyTags' read-back answered with a definite "gone" (recon.IsGone). It unwraps
// to the Pending error it replaces, so it is Pending unless observeJob escalates it.
type outputGoneError struct {
	pending      error
	digest, repo string
}

func (e *outputGoneError) Error() string { return e.pending.Error() }
func (e *outputGoneError) Unwrap() error { return e.pending }

// failureBackoff returns how long to wait after n consecutive failures.
func failureBackoff(n int32) time.Duration {
	d := recon.PendingRetryInterval
	for range n {
		if d >= maxFailureBackoff {
			break
		}
		d *= 2
	}
	return min(d, maxFailureBackoff)
}
