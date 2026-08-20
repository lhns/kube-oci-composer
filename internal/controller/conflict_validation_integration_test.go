//go:build integration

package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// The schema half of onConflict, against a real API server. CEL rules and schema defaults cannot be
// exercised any other way -- a unit test asserting the marker text would be asserting a string, not
// a validation.

// The rule refuses a spec that says two contradictory things, and ONLY that. Allowing the agreeing
// combinations matters as much as refusing the contradictory ones: every object stored before this
// release already carries `immutable: true` materialised by the old schema default, so a rule that
// refused co-presence outright would stop all of them from adopting the new field.
func TestIntegrationOnConflictContradictionsAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name       string
		policy     ociv1alpha1.TagConflictPolicy
		immutable  *bool
		wantRefuse bool
	}{
		{"only-the-new-field", ociv1alpha1.ConflictKeep, nil, false},
		{"only-the-deprecated-field", "", ptr.To(false), false},
		{"neither", "", nil, false},
		{"agreeing-true-and-fail", ociv1alpha1.ConflictFail, ptr.To(true), false},
		{"agreeing-false-and-overwrite", ociv1alpha1.ConflictOverwrite, ptr.To(false), false},
		{"true-but-overwrite", ociv1alpha1.ConflictOverwrite, ptr.To(true), true},
		{"true-but-keep", ociv1alpha1.ConflictKeep, ptr.To(true), true},
		{"false-but-fail", ociv1alpha1.ConflictFail, ptr.To(false), true},
		{"false-but-keep", ociv1alpha1.ConflictKeep, ptr.To(false), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := validBuildSpec()
			spec.Push.OnConflict = tc.policy
			spec.Push.Immutable = tc.immutable

			err := applyBuild(t, "conflict-"+tc.name, spec)
			switch {
			case tc.wantRefuse && err == nil:
				t.Fatalf("a spec saying immutable=%v and onConflict=%q at once was accepted; "+
					"whichever the controller honours, the other is a lie in the object",
					*tc.immutable, tc.policy)
			case !tc.wantRefuse && err != nil:
				t.Fatalf("a valid combination was refused: %v", err)
			case tc.wantRefuse && !strings.Contains(err.Error(), "contradict"):
				t.Errorf("refusal does not explain what is wrong: %v", err)
			}
		})
	}
}

// The enum. A misspelling must be refused at admission rather than silently resolving to Fail
// inside the controller, where the operator would never see it.
func TestIntegrationOnConflictRejectsUnknownValues(t *testing.T) {
	spec := validBuildSpec()
	spec.Push.OnConflict = "keep" // lowercase: not one of the three
	if err := applyBuild(t, "conflict-misspelled", spec); err == nil {
		t.Fatal("an unknown onConflict value was accepted")
	}
}

// onConflict deliberately carries NO schema default, and this is the test that says so on purpose.
//
// Structural defaults are applied when an object is read back from storage, so defaulting it would
// rewrite every existing `immutable: false` object into a refusing one the moment the CRD was
// upgraded -- reversing, silently, a setting its author chose deliberately. The effective default
// lives in ResolveConflictPolicy instead. If someone adds the marker, this fails and the comment
// above is the explanation of why.
func TestIntegrationOnConflictHasNoSchemaDefault(t *testing.T) {
	obj := &ociv1alpha1.ImageBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "conflict-undefaulted", Namespace: "default"},
		Spec:       validBuildSpec(),
	}
	if err := k8s.Create(integrationCtx(t), obj); err != nil {
		t.Fatalf("creating: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(integrationCtx(t), obj) })

	if obj.Spec.Push.OnConflict != "" {
		t.Errorf("onConflict defaulted to %q; a schema default here would rewrite the meaning of "+
			"every stored immutable:false object on upgrade", obj.Spec.Push.OnConflict)
	}
	if obj.Spec.Push.Immutable != nil {
		t.Errorf("immutable defaulted to %v; its default was removed so that an explicit setting "+
			"stays distinguishable from an absent one, which is what the contradiction rule needs",
			*obj.Spec.Push.Immutable)
	}
	// And the resolved answer is still the safe one.
	if got := obj.Spec.Push.ResolveConflictPolicy(); got != ociv1alpha1.ConflictFail {
		t.Errorf("an object that says nothing resolves to %q, want Fail", got)
	}
}

// The same rule has to exist on the composition side. It is a separate CRD generated from a shared
// Go struct, so a marker that failed to propagate would leave one kind unvalidated.
func TestIntegrationOnConflictIsValidatedOnCompositionsToo(t *testing.T) {
	obj := &ociv1alpha1.ImageComposition{
		ObjectMeta: metav1.ObjectMeta{Name: "conflict-composition", Namespace: "default"},
		Spec: ociv1alpha1.ImageCompositionSpec{
			Layers: []ociv1alpha1.Layer{{
				Name:  "core",
				Fetch: &ociv1alpha1.FetchSource{URL: "https://example.com/a.tgz", Digest: validDigest},
				To:    "/x",
			}},
			Publish: &ociv1alpha1.Publish{
				Name:       "app",
				Tags:       []string{"v1"},
				OnConflict: ociv1alpha1.ConflictOverwrite,
				Immutable:  ptr.To(true),
			},
		},
	}
	err := k8s.Create(integrationCtx(t), obj)
	if err == nil {
		t.Cleanup(func() { _ = k8s.Delete(integrationCtx(t), obj) })
		t.Fatal("the contradiction rule did not reach the composition CRD; the two kinds share " +
			"the Go struct but not the generated schema")
	}
	if !strings.Contains(err.Error(), "contradict") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}
