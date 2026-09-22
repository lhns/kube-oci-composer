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
	// watched). It also bounds how long a pushed but untagged image is exposed to the registry's
	// collector (ADR 0054); the chart derives gcDelay from it.
	defaultBuildPollInterval = 15 * time.Second

	// maxFailureBackoff caps the retry interval, so a pushed fix is noticed promptly.
	maxFailureBackoff = 10 * time.Minute
)

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
