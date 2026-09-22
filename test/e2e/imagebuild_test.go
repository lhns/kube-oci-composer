//go:build e2e

// ImageBuild against a real cluster.
//
// Covers what only a cluster can: rootless BuildKit running on the nodes (ADR 0025's second spike
// question; a failure is an answer, so it is loud), the digest returned through the pod's
// termination message, and the FROM check reading a real context over HTTP.
package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	// Must match up.sh's BUILD_NS.
	buildNamespace = "oci-builder-e2e"
	// The INTERNAL registry name. These fixtures set spec.push.repository explicitly, which is used
	// verbatim (ADR 0037), and the push comes from a Job inside the cluster -- so it must be a name
	// cluster DNS resolves, unlike registryHost.
	buildRegistry = "kube-oci-composer-registry.oci-composer.svc.cluster.local:5000"
	// One release, one namespace (ADR 0033).
	operatorNamespace = "oci-composer"
)

// buildTimeout covers a cold build (base pull plus push). It must stay well under `go test
// -timeout` (Makefile), or the binary panics before the diagnostics below are printed.
const buildTimeout = 5 * time.Minute

// refusalTimeout is for outcomes that run NO Job (a conflict refused by pre-flight, a Keep). They
// settle in seconds; the build budget would make a stall look like a slow build.
const refusalTimeout = 90 * time.Second

// buildEventually polls like eventually(), but on timeout dumps the builder's logs, the Jobs and
// the build pods, since a failure is usually inside the build.
func buildEventually(t *testing.T, what string, fn func() error) {
	t.Helper()
	buildEventuallyWithin(t, buildTimeout, what, fn)
}

// buildEventuallyWithin is buildEventually with a caller-chosen budget.
func buildEventuallyWithin(t *testing.T, timeout time.Duration, what string, fn func() error) {
	t.Helper()
	buildEventuallyPolling(t, timeout, interval, what, fn)
}

// buildEventuallyPolling also takes the poll rate, for the one caller whose measurement resolution
// is the poll interval (awaitPublishedDigest).
func buildEventuallyPolling(t *testing.T, timeout, poll time.Duration, what string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if last = fn(); last == nil {
			return
		}
		time.Sleep(poll)
	}

	ctrl, _ := kubectl(t, "-n", operatorNamespace, "logs", "deploy/kube-oci-composer-builder", "--tail=120")
	jobs, _ := kubectl(t, "-n", buildNamespace, "get", "jobs,pods", "-o", "wide")
	pods, _ := kubectl(t, "-n", buildNamespace, "logs", "-l", "job-name", "--tail=120", "--all-containers")
	obj, _ := kubectl(t, "-n", buildNamespace, "get", "imagebuild", "-o", "yaml")
	t.Fatalf("timed out waiting for %s: %v\n\nbuilder logs:\n%s\n\nobjects:\n%s\n\nbuild logs:\n%s\n\nimagebuilds:\n%s",
		what, last, ctrl, jobs, pods, obj)
}

// imageBuildStatus is the part of an ImageBuild's status these tests read.
type imageBuildStatus struct {
	InputHash              string `json:"inputHash"`
	LastHandledReconcileAt string `json:"lastHandledReconcileAt"`
	Artifact               *struct {
		Digest string `json:"digest"`
		Ref    string `json:"ref"`
	} `json:"artifact"`
	Conflict *struct {
		Tag      string `json:"tag"`
		Existing string `json:"existing"`
		Dropped  string `json:"dropped"`
	} `json:"conflict"`
	// Why the last build failed; the durable record, since its pod is deleted by the retry
	// (ADR 0046).
	LastAttempt *struct {
		PodName string `json:"podName"`
		Message string `json:"message"`
	} `json:"lastAttempt"`
	// Past builds, newest first. ADR 0031's guarantee is stated over this list; status.artifact
	// names only the latest build.
	History    []buildRecord     `json:"history"`
	Conditions []statusCondition `json:"conditions"`
}

// buildRecord is the part of a status.history entry these tests read.
type buildRecord struct {
	Digest string   `json:"digest"`
	Tags   []string `json:"tags"`
}

type statusCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func buildStatus(t *testing.T, name string) imageBuildStatus {
	t.Helper()
	out := mustKubectl(t, "-n", buildNamespace, "get", "imagebuild", name,
		"-o", "jsonpath={.status}")
	var st imageBuildStatus
	if strings.TrimSpace(out) == "" {
		return st
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("parsing status of %s: %v\n%s", name, err, out)
	}
	return st
}

// readyCondition returns the Ready condition, or nil. A condition rather than a bool, because
// callers report its reason and message.
func readyCondition(st imageBuildStatus) *statusCondition {
	for i := range st.Conditions {
		if st.Conditions[i].Type == "Ready" {
			return &st.Conditions[i]
		}
	}
	return nil
}

// applyBuild creates an ImageBuild pushing <buildRegistry>/e2e/<name>:v1. extraSpec is appended
// verbatim under spec, already indented.
func applyBuild(t *testing.T, name, dockerfile string, extraSpec ...string) {
	t.Helper()
	applyBuildToTagged(t, name, dockerfile, buildRegistry+"/e2e/"+name, "[v1]", extraSpec...)
}

// applyBuildTo is applyBuild with the repository chosen, so two objects can share one -- the only
// way to produce a real tag conflict.
func applyBuildTo(t *testing.T, name, dockerfile, repository string, extraSpec ...string) {
	t.Helper()
	applyBuildToTagged(t, name, dockerfile, repository, "[v1]", extraSpec...)
}

// applyBuildToUntagged publishes BY DIGEST ONLY (push.tags: []), the mode ADR 0010 tells users to
// consume. The controller still names it after its own digest (ADR 0060).
func applyBuildToUntagged(t *testing.T, name, dockerfile, repository string, extraSpec ...string) {
	t.Helper()
	applyBuildToTagged(t, name, dockerfile, repository, "[]", extraSpec...)
}

// applyBuildToTagged is the shared body; tags is a YAML list literal.
func applyBuildToTagged(t *testing.T, name, dockerfile, repository, tags string, extraSpec ...string) {
	t.Helper()
	applyStdin(t, fmt.Sprintf(`
apiVersion: oci.lhns.de/v1alpha1
kind: ImageBuild
metadata:
  name: %s
  namespace: %s
spec:
  interval: 1h
  context:
    sourceRef:
      kind: GitRepository
      name: e2e-src
  dockerfile: {path: %s}
  platforms: [linux/amd64]
  timeout: 10m
  push:
    repository: %s
    tags: %s
%s
`, name, buildNamespace, dockerfile, repository, tags, strings.Join(extraSpec, "\n")))
	t.Cleanup(func() {
		_, _ = kubectl(t, "-n", buildNamespace, "delete", "imagebuild", name, "--ignore-not-found")
	})
}

// TestImageBuildProducesAnImage -- rootless BuildKit runs, the digest comes back through the
// termination message, and the registry really serves a manifest at that digest.
func TestImageBuildProducesAnImage(t *testing.T) {
	applyBuild(t, "e2e-build", "Dockerfile")

	buildEventually(t, "the build to become Ready", func() error {
		st := buildStatus(t, "e2e-build")
		ready := readyCondition(st)
		switch {
		case ready == nil:
			return fmt.Errorf("no Ready condition yet")
		case ready.Status != "True":
			return fmt.Errorf("Ready=%s (%s): %s", ready.Status, ready.Reason, ready.Message)
		case st.Artifact == nil || st.Artifact.Digest == "":
			return fmt.Errorf("Ready but no artifact digest recorded")
		}
		return nil
	})

	st := buildStatus(t, "e2e-build")
	if !strings.HasPrefix(st.Artifact.Digest, "sha256:") {
		t.Fatalf("digest %q is not a sha256 reference", st.Artifact.Digest)
	}
	if st.InputHash == "" {
		t.Error("no input hash recorded, so the short-circuit has nothing to compare")
	}

	// Proves the push happened, not merely that the controller believes it did.
	manifest := fetchInCluster(t, "verify-digest",
		fmt.Sprintf("http://%s/v2/e2e/e2e-build/manifests/%s", buildRegistry, st.Artifact.Digest))
	if !strings.Contains(manifest, "layers") {
		tags := fetchInCluster(t, "verify-tags",
			fmt.Sprintf("http://%s/v2/e2e/e2e-build/tags/list", buildRegistry))
		t.Fatalf("the registry does not serve a manifest at the recorded digest %s\n"+
			"response:\n%s\ntags the registry does have:\n%s", st.Artifact.Digest, manifest, tags)
	}
}

// TestImageBuildIsIdempotent -- the input hash is the cost model: reconciling an unchanged object
// must not build again (same hash, same digest, no new Job).
func TestImageBuildIsIdempotent(t *testing.T) {
	applyBuild(t, "e2e-idempotent", "Dockerfile")

	buildEventually(t, "the first build to finish", func() error {
		if st := buildStatus(t, "e2e-idempotent"); st.Artifact != nil && st.Artifact.Digest != "" {
			return nil
		}
		return fmt.Errorf("no artifact yet")
	})

	first := buildStatus(t, "e2e-idempotent")
	jobsBefore := jobsFor(t, "e2e-idempotent")

	// Wait for the controller to acknowledge THIS reconcile request rather than sleeping: a sleep
	// would pass while a regressed rebuild simply had not started yet.
	requested := fmt.Sprintf("%d", time.Now().Unix())
	mustKubectl(t, "-n", buildNamespace, "annotate", "imagebuild", "e2e-idempotent",
		"reconcile.fluxcd.io/requestedAt="+requested, "--overwrite")
	buildEventually(t, "the reconcile request to be handled", func() error {
		if got := buildStatus(t, "e2e-idempotent").LastHandledReconcileAt; got != requested {
			return fmt.Errorf("lastHandledReconcileAt = %q, want %q", got, requested)
		}
		return nil
	})

	second := buildStatus(t, "e2e-idempotent")
	if second.InputHash != first.InputHash {
		t.Errorf("input hash moved without an input changing: %q then %q",
			first.InputHash, second.InputHash)
	}
	if second.Artifact == nil || second.Artifact.Digest != first.Artifact.Digest {
		t.Errorf("digest changed on an unchanged spec: %+v then %+v", first.Artifact, second.Artifact)
	}

	if jobsAfter := jobsFor(t, "e2e-idempotent"); len(jobsAfter) > len(jobsBefore) {
		t.Errorf("an unchanged reconcile started another build:\nbefore: %v\nafter:  %v",
			jobsBefore, jobsAfter)
	}
}

// jobsFor returns one build's Jobs (named after the object), so neighbouring tests do not count.
func jobsFor(t *testing.T, name string) []string {
	t.Helper()
	all := mustKubectl(t, "-n", buildNamespace, "get", "jobs", "-o", "jsonpath={.items[*].metadata.name}")
	var mine []string
	for _, j := range strings.Fields(all) {
		if strings.HasPrefix(j, name+"-") {
			mine = append(mine, j)
		}
	}
	return mine
}

// TestImageBuildRefusesAnUnpinnedFrom -- an unpinned base lets an unchanged spec build on something
// else, so it is refused before any Job exists.
func TestImageBuildRefusesAnUnpinnedFrom(t *testing.T) {
	applyBuild(t, "e2e-unpinned", "Dockerfile.unpinned")

	buildEventually(t, "the unpinned FROM to be refused", func() error {
		st := buildStatus(t, "e2e-unpinned")
		if c := readyCondition(st); c != nil && c.Status == "False" &&
			strings.Contains(c.Message, "pinned by digest") {
			return nil
		}
		if st.Artifact != nil {
			return fmt.Errorf("an unpinned FROM produced an artifact: %+v", st.Artifact)
		}
		return fmt.Errorf("not refused yet: %+v", st.Conditions)
	})

	jobs := mustKubectl(t, "-n", buildNamespace, "get", "jobs",
		"-o", "jsonpath={.items[*].metadata.name}")
	if strings.Contains(jobs, "e2e-unpinned") {
		t.Errorf("a Job was created for an unpinned FROM: %s", jobs)
	}
}

// TestRebuildingTheSameContextReproducesTheDigest answers ADR 0025's first spike question: do
// SOURCE_DATE_EPOCH=0 and rewrite-timestamp=true give byte-identical output (see also ADR 0027)?
//
// Two objects rather than one recreated, because a recreated object would adopt the first build's
// finished Job (its name derives from the inputs); and the cache is disabled, or the digests would
// match by reuse rather than reproducibility.
func TestRebuildingTheSameContextReproducesTheDigest(t *testing.T) {
	const noCache = "  cache:\n    mode: Disabled"

	digests := make(map[string]string, 2)
	for _, name := range []string{"e2e-repro-a", "e2e-repro-b"} {
		applyBuild(t, name, "Dockerfile", noCache)

		buildEventually(t, "build "+name+" to finish", func() error {
			st := buildStatus(t, name)
			if st.Artifact == nil || st.Artifact.Digest == "" {
				return fmt.Errorf("no artifact yet: %+v", st.Conditions)
			}
			return nil
		})
		digests[name] = buildStatus(t, name).Artifact.Digest
	}

	a, b := digests["e2e-repro-a"], digests["e2e-repro-b"]
	if a == "" || b == "" {
		t.Fatalf("a build produced no digest: %q and %q", a, b)
	}
	if a != b {
		// Not a flake: ADR 0025 asks that this be recorded rather than smoothed over.
		t.Fatalf("two builds of an identical context produced different digests:\n  %s\n  %s\n"+
			"status.inputHash therefore identifies the inputs and not the output, so a rebuild "+
			"after losing status or the store can permanently conflict with an already-published "+
			"immutable tag (ADR 0025)", a, b)
	}
}

// TestImageBuildTagConflictPolicy checks each onConflict policy (ADR 0029) against what the
// registry actually serves, with two objects pointed at one repository and tag.
func TestImageBuildTagConflictPolicy(t *testing.T) {
	repo := fmt.Sprintf("%s/e2e/e2e-conflict", buildRegistry)

	// The incumbent; every assertion compares against its digest.
	applyBuildTo(t, "e2e-conflict-first", "Dockerfile", repo)
	buildEventually(t, "the first build to publish", func() error {
		st := buildStatus(t, "e2e-conflict-first")
		if st.Artifact == nil || st.Artifact.Digest == "" {
			return fmt.Errorf("no digest yet: %+v", readyCondition(st))
		}
		return nil
	})
	original := buildStatus(t, "e2e-conflict-first").Artifact.Digest

	// Different content, same tag, default policy: refused, without a Job (a push cannot be undone).
	applyBuildTo(t, "e2e-conflict-fail", "Dockerfile.other", repo)
	buildEventuallyWithin(t, refusalTimeout, "the conflicting build to be refused", func() error {
		st := buildStatus(t, "e2e-conflict-fail")
		ready := readyCondition(st)
		if ready == nil || ready.Status != "False" {
			return fmt.Errorf("Ready=%+v, want False", ready)
		}
		if !strings.Contains(ready.Message, "already resolves to") {
			return fmt.Errorf("refused for the wrong reason: %s", ready.Message)
		}
		return nil
	})

	if got := tagDigest(t, "e2e/e2e-conflict", "v1"); got != original {
		t.Fatalf("the tag moved to %s despite the build being refused; it should still be %s",
			got, original)
	}

	// Keep: tag untouched, no build, Ready -- and the divergence recorded in status.
	applyBuildTo(t, "e2e-conflict-keep", "Dockerfile.other", repo, "    onConflict: Keep")
	buildEventuallyWithin(t, refusalTimeout, "the kept build to report Ready", func() error {
		st := buildStatus(t, "e2e-conflict-keep")
		ready := readyCondition(st)
		if ready == nil || ready.Status != "True" {
			return fmt.Errorf("Ready=%+v, want True", ready)
		}
		if st.Conflict == nil {
			return fmt.Errorf("Ready but nothing records what was kept")
		}
		return nil
	})

	kept := buildStatus(t, "e2e-conflict-keep")
	if kept.Conflict.Tag != "v1" || kept.Conflict.Existing != original {
		t.Errorf("conflict = %+v, want tag v1 at %s", kept.Conflict, original)
	}
	if got := tagDigest(t, "e2e/e2e-conflict", "v1"); got != original {
		t.Errorf("Keep moved the tag to %s; it must be left at %s", got, original)
	}

	// Overwrite really moves it.
	applyBuildTo(t, "e2e-conflict-overwrite", "Dockerfile.other", repo, "    onConflict: Overwrite")
	buildEventually(t, "the overwriting build to publish", func() error {
		st := buildStatus(t, "e2e-conflict-overwrite")
		if st.Artifact == nil || st.Artifact.Digest == "" {
			return fmt.Errorf("no digest yet: %+v", readyCondition(st))
		}
		if st.Artifact.Digest == original {
			return fmt.Errorf("built the same digest as the incumbent, so this proves nothing " +
				"about overwriting; the two Dockerfiles must differ")
		}
		return nil
	})
	moved := buildStatus(t, "e2e-conflict-overwrite").Artifact.Digest
	if got := tagDigest(t, "e2e/e2e-conflict", "v1"); got != moved {
		t.Errorf("the tag is at %s after an Overwrite that produced %s", got, moved)
	}
}

// tagDigest asks the registry what a tag resolves to. It reads Docker-Content-Digest rather than
// hashing the body, whose representation depends on the Accept header.
func tagDigest(t *testing.T, repository, tag string) string {
	t.Helper()
	out := headersInCluster(t, "tag-digest",
		fmt.Sprintf("http://%s/v2/%s/manifests/%s", buildRegistry, repository, tag))
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), "Docker-Content-Digest") {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("no Docker-Content-Digest for %s:%s\n%s", repository, tag, out)
	return ""
}
