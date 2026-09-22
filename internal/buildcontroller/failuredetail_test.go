package buildcontroller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestTheBuildContainerCopiesItsLogTail pins FallbackToLogsOnError on every container: buildctl
// never writes /dev/termination-log on failure, so without it status carries no cause.
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

// TestTheCauseLeads: the cause comes first, so truncation cannot eat it.
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

// TestALongCauseKeepsItsEndAndFits: over 4096 bytes the API server rejects the whole status write,
// so the message must fit while keeping the log's end, where the error is.
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
