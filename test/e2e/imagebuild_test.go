//go:build e2e

// ImageBuild against a real cluster.
//
// This is the first thing that runs the builder end to end, and it exists because two pieces of it
// could not be verified any other way. The digest comes back out of the build through the pod's
// termination message, which needs a real kubelet to populate; and the FROM check reads a real
// context over HTTP. Both were previously only unit-tested against fakes.
//
// It also answers ADR 0025's second spike question — whether rootless BuildKit runs on the target
// nodes at all. If it does not, that is a real answer and the ADR lists it as grounds to abandon,
// so the failure must be loud rather than skipped.
package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// buildNamespace must match up.sh's BUILD_NS, which defaults to the same value. The registry host
// is derived from it rather than written out twice, so overriding one cannot leave the other
// pointing somewhere the fixtures are not.
const (
	buildNamespace = "oci-builder-e2e"
	buildRegistry  = "e2e-registry." + buildNamespace + ".svc.cluster.local:5000"
)

// buildTimeout is longer than the composer's `timeout` because a cold build pulls a base image and
// pushes a result. It must stay well under `go test -timeout` (see the Makefile) or the binary
// panics first and the dump below never runs.
const buildTimeout = 5 * time.Minute

// buildEventually polls like eventually(), but dumps the BUILDER's logs and the build pod's on
// timeout, since a failure is usually inside the build rather than in the controller.
func buildEventually(t *testing.T, what string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(buildTimeout)
	var last error
	for time.Now().Before(deadline) {
		if last = fn(); last == nil {
			return
		}
		time.Sleep(interval)
	}

	ctrl, _ := kubectl(t, "-n", "oci-builder", "logs", "deploy/kube-oci-builder", "--tail=120")
	jobs, _ := kubectl(t, "-n", buildNamespace, "get", "jobs,pods", "-o", "wide")
	pods, _ := kubectl(t, "-n", buildNamespace, "logs", "-l", "job-name", "--tail=120", "--all-containers")
	obj, _ := kubectl(t, "-n", buildNamespace, "get", "imagebuild", "-o", "yaml")
	t.Fatalf("timed out waiting for %s: %v\n\nbuilder logs:\n%s\n\nobjects:\n%s\n\nbuild logs:\n%s\n\nimagebuilds:\n%s",
		what, last, ctrl, jobs, pods, obj)
}

// dockerBuildStatus is the part of status this test reads.
type dockerBuildStatus struct {
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
	Conditions []statusCondition `json:"conditions"`
}

type statusCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func buildStatus(t *testing.T, name string) dockerBuildStatus {
	t.Helper()
	out := mustKubectl(t, "-n", buildNamespace, "get", "imagebuild", name,
		"-o", "jsonpath={.status}")
	var st dockerBuildStatus
	if strings.TrimSpace(out) == "" {
		return st
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("parsing status of %s: %v\n%s", name, err, out)
	}
	return st
}

// readyCondition returns the Ready condition, or nil. It returns the condition rather than a bool
// because every caller wants the reason and message when it is not what they expected — which is
// why a bool-returning version kept being bypassed.
func readyCondition(st dockerBuildStatus) *statusCondition {
	for i := range st.Conditions {
		if st.Conditions[i].Type == "Ready" {
			return &st.Conditions[i]
		}
	}
	return nil
}

// applyBuild creates an ImageBuild. extraSpec is appended verbatim under spec, already indented
// two spaces, for the fields only one test needs.
func applyBuild(t *testing.T, name, dockerfile string, extraSpec ...string) {
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
    kind: GitRepository
    name: e2e-src
  dockerfile: %s
  platforms: [linux/amd64]
  timeout: 10m
  push:
    repository: %s/e2e/%s
    tags: [v1]
%s
`, name, buildNamespace, dockerfile, buildRegistry, name, strings.Join(extraSpec, "\n")))
	t.Cleanup(func() {
		_, _ = kubectl(t, "-n", buildNamespace, "delete", "imagebuild", name, "--ignore-not-found")
	})
}

// applyBuildTo is applyBuild with the push target chosen, so two objects can be pointed at one
// repository -- which is the only way to produce a real tag conflict.
func applyBuildTo(t *testing.T, name, dockerfile, repository string, extraSpec ...string) {
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
    kind: GitRepository
    name: e2e-src
  dockerfile: %s
  platforms: [linux/amd64]
  timeout: 10m
  push:
    repository: %s
    tags: [v1]
%s
`, name, buildNamespace, dockerfile, repository, strings.Join(extraSpec, "\n")))
	t.Cleanup(func() {
		_, _ = kubectl(t, "-n", buildNamespace, "delete", "imagebuild", name, "--ignore-not-found")
	})
}

// TestImageBuildProducesAnImage is the whole point of the kind.
//
// It proves the three things only a cluster can: rootless BuildKit runs, the built digest makes it
// back through the pod's termination message, and the image is actually in the registry afterwards.
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

	// The digest must name something the registry actually has. This is what proves the push
	// happened rather than the controller merely believing it did.
	manifest := curlInCluster(t, "verify-digest",
		fmt.Sprintf("http://%s/v2/e2e/e2e-build/manifests/%s", buildRegistry, st.Artifact.Digest))
	if !strings.Contains(manifest, "layers") {
		tags := curlInCluster(t, "verify-tags",
			fmt.Sprintf("http://%s/v2/e2e/e2e-build/tags/list", buildRegistry))
		t.Fatalf("the registry does not serve a manifest at the recorded digest %s\n"+
			"response:\n%s\ntags the registry does have:\n%s", st.Artifact.Digest, manifest, tags)
	}
}

// curlInCluster fetches a URL from inside the cluster and returns the body.
//
// Deliberately not `kubectl run --rm -i`, which attaches AFTER creating the pod: a container that
// makes one request and exits finishes before the attach lands, so kubectl returns success with no
// output and the assertion reads it as an empty response. Creating the pod, waiting for it to
// finish and then reading its log has no such race.
//
// The request asks for every media type containerd would, because BuildKit pushes Docker media
// types unless told otherwise, and accepting only the OCI ones would fail on a correct manifest.
func curlInCluster(t *testing.T, name, url string) string {
	t.Helper()

	const accept = "application/vnd.oci.image.manifest.v1+json," +
		"application/vnd.oci.image.index.v1+json," +
		"application/vnd.docker.distribution.manifest.v2+json," +
		"application/vnd.docker.distribution.manifest.list.v2+json,*/*"

	_, _ = kubectl(t, "-n", buildNamespace, "delete", "pod", name, "--ignore-not-found")
	mustKubectl(t, "-n", buildNamespace, "run", name,
		"--restart=Never", "--image=busybox:1.37", "--command", "--",
		"wget", "-qO-", "--header", "Accept: "+accept, url)
	t.Cleanup(func() {
		_, _ = kubectl(t, "-n", buildNamespace, "delete", "pod", name, "--ignore-not-found")
	})

	// Succeeded or Failed: a non-200 leaves wget's exit code behind and an empty log, and the
	// caller's assertion reports that better than a timeout here would.
	for _, cond := range []string{"Succeeded", "Failed"} {
		if _, err := kubectl(t, "-n", buildNamespace, "wait", "--for=jsonpath={.status.phase}="+cond,
			"pod/"+name, "--timeout=90s"); err == nil {
			break
		}
	}
	out, _ := kubectl(t, "-n", buildNamespace, "logs", name)
	return out
}

// curlHeaders fetches only the response headers from inside the cluster.
//
// Same create/wait/logs shape as curlInCluster, and for the same reason: `kubectl run --rm -i`
// attaches after the pod is created, so a container that makes one request and exits finishes
// before the attach lands and the output is lost.
//
// `wget -S --spider` sends a HEAD and prints the headers on stderr, which is where the digest a
// registry reports for a tag lives.
func curlHeaders(t *testing.T, name, url string) string {
	t.Helper()

	const accept = "application/vnd.oci.image.manifest.v1+json," +
		"application/vnd.oci.image.index.v1+json," +
		"application/vnd.docker.distribution.manifest.v2+json," +
		"application/vnd.docker.distribution.manifest.list.v2+json,*/*"

	_, _ = kubectl(t, "-n", buildNamespace, "delete", "pod", name, "--ignore-not-found")
	mustKubectl(t, "-n", buildNamespace, "run", name,
		"--restart=Never", "--image=busybox:1.37", "--command", "--",
		"wget", "-S", "--spider", "--header", "Accept: "+accept, url)
	t.Cleanup(func() {
		_, _ = kubectl(t, "-n", buildNamespace, "delete", "pod", name, "--ignore-not-found")
	})

	for _, cond := range []string{"Succeeded", "Failed"} {
		if _, err := kubectl(t, "-n", buildNamespace, "wait", "--for=jsonpath={.status.phase}="+cond,
			"pod/"+name, "--timeout=90s"); err == nil {
			break
		}
	}
	out, _ := kubectl(t, "-n", buildNamespace, "logs", name)
	return out
}

// TestImageBuildIsIdempotent — the input hash is the whole cost model. A second reconcile of an
// unchanged object must not build again, which is visible as the digest and hash both holding still
// while no new Job appears.
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

	// Force a reconcile without changing an input, then wait for the controller to say it handled
	// THAT request rather than sleeping and hoping. A fixed sleep would pass even if the
	// short-circuit had regressed and a rebuild simply had not started yet.
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

// jobsFor returns the Jobs belonging to one build. Scoped by name because the Job name is derived
// from the object's, so a namespace-wide count would be coupled to what neighbouring tests leave.
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

// TestImageBuildRefusesAnUnpinnedFrom — the one rule the controller enforces on a Dockerfile's
// CONTENT, and the reason it is enforced before a Job exists: an unpinned base means an unchanged
// spec can silently build on something else.
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

	// And no Job may exist for it — the check has to happen before anything executes.
	jobs := mustKubectl(t, "-n", buildNamespace, "get", "jobs",
		"-o", "jsonpath={.items[*].metadata.name}")
	if strings.Contains(jobs, "e2e-unpinned") {
		t.Errorf("a Job was created for an unpinned FROM: %s", jobs)
	}
}

// TestRebuildingTheSameContextReproducesTheDigest answers ADR 0025's first spike question: does
// SOURCE_DATE_EPOCH=0 plus rewrite-timestamp=true give byte-identical output across two runs of the
// same context? See 0025 and 0027 for what each answer costs.
//
// Two things keep it from passing vacuously. Two objects rather than one deleted and recreated,
// because the Job name is derived from the inputs and a recreated object would ADOPT the first
// build's finished Job and read its digest back without building. And the cache disabled on both,
// or the second digest would match by reuse rather than by reproducibility.
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
		// Not a flake to retry. This is 0025's concession reproducing, and the ADR asks that it be
		// recorded rather than smoothed over: an unchanged spec can produce two different images.
		t.Fatalf("two builds of an identical context produced different digests:\n  %s\n  %s\n"+
			"status.inputHash therefore identifies the inputs and not the output, so a rebuild "+
			"after losing status or the store can permanently conflict with an already-published "+
			"immutable tag (ADR 0025)", a, b)
	}
}

// The tag-conflict policy, end to end and against a real registry.
//
// This is the assertion that could not have existed before ADR 0029: `push.immutable` was in this
// kind's CRD from the day it shipped and nothing read it, so BuildKit overwrote whatever the tag
// held. Two objects are pointed at one repository, which is the only way to produce a genuine
// conflict, and each policy is then checked against what the registry actually serves.
func TestImageBuildTagConflictPolicy(t *testing.T) {
	repo := fmt.Sprintf("%s/e2e/e2e-conflict", buildRegistry)

	// The incumbent. Its digest is what every assertion below compares against.
	applyBuildTo(t, "e2e-conflict-first", "Dockerfile", repo)
	buildEventually(t, "the first build to publish", func() error {
		st := buildStatus(t, "e2e-conflict-first")
		if st.Artifact == nil || st.Artifact.Digest == "" {
			return fmt.Errorf("no digest yet: %+v", readyCondition(st))
		}
		return nil
	})
	original := buildStatus(t, "e2e-conflict-first").Artifact.Digest

	// A second object, different content, same tag. Under the default it must refuse -- and it must
	// refuse without ever creating a Job, because a push from inside one cannot be undone.
	applyBuildTo(t, "e2e-conflict-fail", "Dockerfile.other", repo)
	buildEventually(t, "the conflicting build to be refused", func() error {
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

	// Keep leaves the tag alone, runs no build, reports Ready -- and records the divergence, which
	// is the only thing separating this from a silent one.
	applyBuildTo(t, "e2e-conflict-keep", "Dockerfile.other", repo, "    onConflict: Keep")
	buildEventually(t, "the kept build to report Ready", func() error {
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

	// And Overwrite really does move it, or the whole enum would be one behaviour with three names.
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

// tagDigest asks the registry what a tag resolves to, from inside the cluster.
//
// Reads the Docker-Content-Digest header rather than hashing the body, because the body a registry
// returns depends on the media types requested and hashing the wrong representation would compare
// two different digests of the same image.
func tagDigest(t *testing.T, repository, tag string) string {
	t.Helper()
	out := curlHeaders(t, "tag-digest",
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
