package controller

import (
	"context"
	"strings"
	"testing"

	"k8s.io/client-go/tools/record"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// TestAnUnpinnedSourceUnderFailIsAnnounced — a tag that cannot move, fed by a source that can,
// can wedge: a build before the source catches up publishes the old revision under the new tag,
// and Fail refuses the corrective build (ADR 0052). Pinning is covered by the wiring test below.
func TestAnUnpinnedSourceUnderFailIsAnnounced(t *testing.T) {
	for _, tc := range []struct {
		name string
		push *ociv1alpha1.Push
		want bool
	}{
		{
			name: "Fail, tagged",
			push: &ociv1alpha1.Push{Tags: []string{"s0eff05b20f86b0e9"}},
			want: true,
		},
		{
			// A policy that tolerates a second answer cannot wedge.
			name: "Overwrite",
			push: &ociv1alpha1.Push{Tags: []string{"v1"}, OnConflict: ociv1alpha1.ConflictOverwrite},
			want: false,
		},
		{
			name: "Keep",
			push: &ociv1alpha1.Push{Tags: []string{"v1"}, OnConflict: ociv1alpha1.ConflictKeep},
			want: false,
		},
		{
			// Digest-only: a different revision gets a different name and collides with nothing.
			name: "Fail, digest-only",
			push: &ociv1alpha1.Push{Repository: "ghcr.io/me/app"},
			want: false,
		},
		{
			// No push block resolves to Fail, digest-only.
			name: "no push block",
			push: nil,
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := record.NewFakeRecorder(8)

			obj := &ociv1alpha1.ImageComposition{}
			obj.Name, obj.Namespace = "app", "team-a"
			obj.Spec.Push = tc.push

			warnUnpinnedUnderFail(rec, obj, &ociv1alpha1.SourceRefSource{
				Kind: "GitRepository", Name: "ext",
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
			// Actionable: which source, and what to do about it.
			for _, want := range []string{"GitRepository/ext", "revision:"} {
				if !strings.Contains(got, want) {
					t.Errorf("event does not mention %q: %s", want, got)
				}
			}
		})
	}
}

// TestTheUnpinnedWarningIsActuallyWired drives resolveInputs, proving warnUnpinnedUnderFail is
// called at all. Pinning is covered here because the revision check lives at the call site.
func TestTheUnpinnedWarningIsActuallyWired(t *testing.T) {
	for _, tc := range []struct {
		name     string
		revision string
		want     bool
	}{
		{"unpinned", "", true},
		// The pin ties the tag to the content, so there is nothing left to warn about.
		{"pinned", "main@sha1:abcd", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, digest := contentServer(t, map[string]string{"config/app.conf": "x"})
			repo := gitRepository("platform-config", "default", url, digest, "main@sha1:abcd")

			obj := unpinnedComposition()
			obj.Spec.Layers[0].SourceRef.Revision = tc.revision
			// The shared fixture's immutable: false resolves to Overwrite, which cannot wedge; use
			// the default policy instead.
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
			if found != tc.want {
				t.Errorf("warning raised = %v, want %v", found, tc.want)
			}
		})
	}
}
