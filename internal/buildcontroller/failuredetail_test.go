package buildcontroller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestTheBuildContainerCopiesItsLogTail is the whole of this change in one assertion.
//
// ReadFile takes the termination message only from /dev/termination-log, which buildctl never
// writes -- so status carried boilerplate and a pointer to a pod the next retry deletes. The
// fetcher has had FallbackToLogsOnError all along, which is exactly why ITS failures explained
// themselves and the build container's did not.
func TestTheBuildContainerCopiesItsLogTail(t *testing.T) {
	job := buildJob(sampleBuild(), testHash, "https://example/ctx.tgz", "sha256:ctx", sampleConfig(),
		sampleRepo, "", "", "", "", true)

	pod := job.Spec.Template.Spec
	for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		if c.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
			t.Errorf("container %q has policy %q; a failure there cannot reach status",
				c.Name, c.TerminationMessagePolicy)
		}
	}
}

// terminated builds a pod whose named container died with the given message.
func terminated(name, message string, exit int32) corev1.Pod {
	return corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: name,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: exit, Reason: "Error", Message: message,
				}},
			}},
		},
	}
}

// TestTheCauseLeads — it used to come last, so every truncation ate the one part worth reading and
// left the mechanism behind.
func TestTheCauseLeads(t *testing.T) {
	const cause = "/bin/sh: go: not found"
	got := failureDetailFor("BackoffLimitExceeded", terminated("build", cause, 127))

	if !strings.HasPrefix(got, cause) {
		t.Errorf("the message does not lead with the cause:\n%s", got)
	}
	if !strings.Contains(got, `container "build" exited 127`) {
		t.Errorf("the mechanism was dropped entirely:\n%s", got)
	}
}

// TestALongCauseKeepsItsEndAndFits is the assertion that matters in a cluster.
//
// BuildAttempt.Message caps at 4096 and an over-long value does not truncate -- the API server
// REJECTS the status write, so the failure is lost rather than shortened. A test that only checked
// "contains the cause" would pass while that happened.
func TestALongCauseKeepsItsEndAndFits(t *testing.T) {
	const ending = "ERROR: failed to solve: process did not complete successfully"
	cause := strings.Repeat("pulling layer abcdef0123456789\n", 200) + ending

	got := failureDetailFor("BackoffLimitExceeded", terminated("build", cause, 1))

	if len(got) > maxFailureDetail {
		t.Errorf("message is %d bytes, over the %d the field allows: the status write would fail",
			len(got), maxFailureDetail)
	}
	if !strings.Contains(got, ending) {
		t.Error("the END of the log was cut, which is where a build's error is")
	}
}
