package controller

import (
	"context"
	"strings"
	"testing"

	"k8s.io/client-go/tools/record"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// TestAnUnpinnedSourceUnderFailIsAnnounced covers the combination behind a field incident: a tag
// that cannot move fed by a source that can.
//
// The composition's spec-hash tag is computed by the consumer and lands with the spec, while the
// GitRepository it names catches up separately. A build started in that window publishes the
// PREVIOUS revision under the new tag, and because provenance records the source revision on the
// manifest, the corrective build is GUARANTEED to produce a different digest -- so onConflict: Fail
// refuses it and the object is wedged with no retry that can clear it. ADR 0052.
//
// Deliberately a warning rather than a refusal: tracking a branch is legitimate and ADR 0026 left
// the pin optional on purpose. What was missing is that nothing said so until after it broke.
func TestAnUnpinnedSourceUnderFailIsAnnounced(t *testing.T) {
	for _, tc := range []struct {
		name     string
		revision string
		push     *ociv1alpha1.Push
		want     bool
	}{
		{
			name: "unpinned, Fail, tagged",
			push: &ociv1alpha1.Push{Tags: []string{"s0eff05b20f86b0e9"}},
			want: true,
		},
		{
			// The pin is what ties the tag to the content, so there is nothing left to warn about.
			name:     "pinned",
			revision: "sha1:882834f",
			push:     &ociv1alpha1.Push{Tags: []string{"s0eff05b20f86b0e9"}},
			want:     false,
		},
		{
			// A policy that tolerates a second answer cannot wedge.
			name: "unpinned, Overwrite",
			push: &ociv1alpha1.Push{Tags: []string{"v1"}, OnConflict: ociv1alpha1.ConflictOverwrite},
			want: false,
		},
		{
			name: "unpinned, Keep",
			push: &ociv1alpha1.Push{Tags: []string{"v1"}, OnConflict: ociv1alpha1.ConflictKeep},
			want: false,
		},
		{
			// The one structurally safe configuration: the name IS the content, so a build from a
			// different revision gets a different name and collides with nothing. Warning here
			// would be noise on the case that cannot fail.
			name: "unpinned, Fail, digest-only",
			push: &ociv1alpha1.Push{Repository: "ghcr.io/me/app"},
			want: false,
		},
		{
			// No push block at all resolves to Fail, and publishes by digest. Same reasoning.
			name: "no push block",
			push: nil,
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := record.NewFakeRecorder(8)
			r := &ImageCompositionReconciler{Recorder: rec}

			obj := &ociv1alpha1.ImageComposition{}
			obj.Name, obj.Namespace = "app", "team-a"
			obj.Spec.Push = tc.push

			r.warnIfUnpinnedUnderFail(obj, &ociv1alpha1.SourceRefSource{
				Kind: "GitRepository", Name: "ext", Revision: tc.revision,
			})

			var got string
			select {
			case got = <-rec.Events:
			default:
			}

			if tc.want && got == "" {
				t.Fatal("no event: an unpinned source under a tag that cannot move is how a " +
					"composition wedges, and nothing on the object would have said so")
			}
			if !tc.want && got != "" {
				t.Fatalf("unexpected event on a configuration that cannot wedge: %s", got)
			}
			if !tc.want {
				return
			}
			if !strings.Contains(got, ociv1alpha1.ReasonUnpinnedSource) {
				t.Errorf("event = %q, want it to name %s", got, ociv1alpha1.ReasonUnpinnedSource)
			}
			// The message has to be actionable: which source, and what to do about it.
			for _, want := range []string{"GitRepository/ext", "revision:"} {
				if !strings.Contains(got, want) {
					t.Errorf("event does not mention %q: %s", want, got)
				}
			}
		})
	}
}

// TestTheUnpinnedWarningIsActuallyWired is the half the table above cannot cover.
//
// Every case there calls warnIfUnpinnedUnderFail directly, so all six would pass unchanged if
// nothing ever called it. This drives resolveInputs, which is the path a reconcile takes.
func TestTheUnpinnedWarningIsActuallyWired(t *testing.T) {
	url, digest := tarball(t, map[string]string{"config/app.conf": "x"})
	repo := gitRepository("platform-config", "default", url, digest, "main@sha1:abcd")

	obj := unpinnedComposition()
	// The shared fixture publishes with immutable: false, which resolves to Overwrite and cannot
	// wedge. The incident's configuration is the default policy over a moving source.
	obj.Spec.Push = &ociv1alpha1.Push{Tags: []string{"s0eff05b20f86b0e9"}}

	r := reconcilerWith(t, repo)
	if _, _, err := r.resolveInputs(context.Background(), obj, t.TempDir()); err != nil {
		t.Fatalf("resolving: %v", err)
	}

	rec, ok := r.Recorder.(*record.FakeRecorder)
	if !ok {
		t.Fatal("expected a fake recorder")
	}
	var found bool
	for {
		select {
		case ev := <-rec.Events:
			if strings.Contains(ev, ociv1alpha1.ReasonUnpinnedSource) {
				found = true
			}
			continue
		default:
		}
		break
	}
	if !found {
		t.Error("resolving an unpinned sourceRef under onConflict: Fail raised no warning, so " +
			"the object gives no sign of the one configuration that wedges it")
	}
}
