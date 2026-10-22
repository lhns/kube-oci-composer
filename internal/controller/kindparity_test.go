package controller

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two kinds are separate components (ADR 0004) but must answer the same questions the same
// way: which failures stall, how references are scoped, how messages are recorded. These tests are
// deliberately structural (they read both packages' source); behaviour is tested per kind.

// packageOf names which controller a source file belongs to. The composer's package is read from
// ".", so filepath.Dir is no help.
func packageOf(file string) string {
	if strings.Contains(filepath.ToSlash(file), "buildcontroller") {
		return "buildcontroller"
	}
	return "controller"
}

// controllerSources returns the non-test Go source of both controller packages.
func controllerSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range []string{".", filepath.Join("..", "buildcontroller")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("reading %s: %v", name, err)
			}
			out[filepath.Join(dir, name)] = string(b)
		}
	}
	return out
}

// TestBothKindsScopeReferencesToTheirOwnNamespace — enforced in code because CEL cannot read
// metadata.namespace, so only this checks both controllers do it.
func TestBothKindsScopeReferencesToTheirOwnNamespace(t *testing.T) {
	want := map[string]string{
		"resolve.go":               "the composer's layer sourceRef",
		"imagebuild_controller.go": "the builder's build context",
	}
	for file, body := range controllerSources(t) {
		base := filepath.Base(file)
		what, ok := want[base]
		if !ok {
			continue
		}
		if !strings.Contains(body, "!= obj.Namespace") {
			t.Errorf("%s does not refuse a reference outside its own namespace: %s is the one "+
				"tenancy boundary a spec can cross on its own", base, what)
		}
		delete(want, base)
	}
	for base, what := range want {
		t.Errorf("%s was not found, so %s is unchecked", base, what)
	}
}

// TestBothKindsHonourAPinnedRevision — sourceRef.revision and spec.context.revision are the same
// field on the same type, so both kinds must honour it.
func TestBothKindsHonourAPinnedRevision(t *testing.T) {
	found := map[string]bool{}
	for file, body := range controllerSources(t) {
		if strings.Contains(body, "RevisionMatches(") {
			found[packageOf(file)] = true
		}
	}
	for _, pkg := range []string{"controller", "buildcontroller"} {
		if !found[pkg] {
			t.Errorf("internal/%s never calls RevisionMatches, so a pinned revision is silently "+
				"ignored there while the other kind enforces it", pkg)
		}
	}
}

// TestNeitherKindKeepsItsOwnCopyOfTheSharedHelpers — the plumbing both loops need lives in
// internal/reconciler; a local copy in either controller can drift.
func TestNeitherKindKeepsItsOwnCopyOfTheSharedHelpers(t *testing.T) {
	shared := map[string]bool{
		"setCondition": true, "removeCondition": true, "truncate": true,
		"terminal": true, "pending": true, "isTerminal": true, "isPending": true,
		"recordHistory": true, "interval": true,
		"publishedState": true, "resolvePublished": true, "keychainFromSecret": true,
		"normaliseHost": true,
	}

	fset := token.NewFileSet()
	for file, body := range controllerSources(t) {
		f, err := parser.ParseFile(fset, file, body, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			if shared[fn.Name.Name] {
				t.Errorf("%s declares %s, which internal/reconciler already provides: two copies "+
					"is how the two kinds drift apart", filepath.Base(file), fn.Name.Name)
			}
		}
	}
}

// TestBothKindsRecordEventsThroughTheSharedHelper — recon.Event truncates, and the API server
// rejects an over-long event outright.
func TestBothKindsRecordEventsThroughTheSharedHelper(t *testing.T) {
	for file, body := range controllerSources(t) {
		if !strings.Contains(body, "Recorder.Event(") {
			continue
		}
		t.Errorf("%s calls Recorder.Event directly instead of recon.Event, which is what applies "+
			"the length limit", filepath.Base(file))
	}
}

// TestBothKindsHonourOnConflict — the policy is the same field on both kinds and must be enforced
// on both (ADR 0029). Catches enforcement deleted from one side while the field stays in the schema.
func TestBothKindsHonourOnConflict(t *testing.T) {
	want := map[string]string{
		"imagecomposition_controller.go": "the composer, which checks before writing any tag",
		"conflict.go":                    "the builder, which checks before the Job is created",
	}
	for file, body := range controllerSources(t) {
		base := filepath.Base(file)
		what, ok := want[base]
		if !ok {
			continue
		}
		if !strings.Contains(body, "ResolveConflictPolicy()") {
			t.Errorf("%s never resolves the conflict policy: %s", base, what)
		}
		for _, v := range []string{"ConflictFail", "ConflictKeep"} {
			if !strings.Contains(body, v) {
				t.Errorf("%s does not handle %s; a value in the enum that one kind ignores is a "+
					"guarantee its CRD advertises and does not provide", base, v)
			}
		}
		delete(want, base)
	}
	for base, what := range want {
		t.Errorf("%s was not read at all (%s); if it moved, point this test at the new file "+
			"rather than dropping the assertion", base, what)
	}
}

// TestBothKindsNameEverythingAfterItsDigest — ADR 0060 must hold on both kinds: with keepUntagged
// off, an untagged manifest from a kind that forgot is reclaimed under a live object.
func TestBothKindsNameEverythingAfterItsDigest(t *testing.T) {
	want := map[string]bool{"controller": false, "buildcontroller": false}
	backfill := map[string]bool{"controller": false, "buildcontroller": false}
	for file, body := range controllerSources(t) {
		pkg := packageOf(file)
		if _, ok := want[pkg]; !ok {
			continue
		}
		if strings.Contains(body, "recon.PublishTags(") {
			want[pkg] = true
		}
		if strings.Contains(body, "backfillDigestTags(") {
			backfill[pkg] = true
		}
	}
	for pkg, ok := range want {
		if !ok {
			t.Errorf("%s never publishes the digest's own tag; its untagged output is reclaimed by "+
				"age once keepUntagged is off", pkg)
		}
	}
	for pkg, ok := range backfill {
		if !ok {
			t.Errorf("%s never backfills the digest's own tag, so objects published before ADR "+
				"0060 stay unprotected until they next change", pkg)
		}
	}
}

// TestBothKindsRecordAKeptTagInStatus — under Keep an object is Ready without publishing what its
// spec produces; without a status record that is a silent divergence (ADR 0026).
func TestBothKindsRecordAKeptTagInStatus(t *testing.T) {
	found := map[string]bool{}
	for file, body := range controllerSources(t) {
		if strings.Contains(body, "TagConflictStatus{") {
			found[packageOf(file)] = true
		}
	}
	for _, pkg := range []string{"controller", "buildcontroller"} {
		if !found[pkg] {
			t.Errorf("%s never constructs a TagConflictStatus: under onConflict: Keep it would "+
				"report Ready while publishing something other than what its spec produces, and "+
				"nothing would say so", pkg)
		}
	}
}

// TestNeitherBinaryCachesSecrets — both controllers have RBAC to get Secrets but not list/watch.
// A cached client watches the type it Gets, so a cached Secret read fails at the reflector: the
// controller looks healthy and never reconciles.
func TestNeitherBinaryCachesSecrets(t *testing.T) {
	for _, main := range []string{"../../cmd/oci-composer/main.go", "../../cmd/oci-builder/main.go"} {
		body, err := os.ReadFile(main)
		if err != nil {
			t.Fatalf("reading %s: %v", main, err)
		}
		if !strings.Contains(string(body), "DisableFor: []client.Object{&corev1.Secret{}}") {
			t.Errorf("%s does not disable the Secret cache. Its RBAC grants get and not list, so "+
				"the cache cannot start -- and the controller will look healthy while reconciling "+
				"nothing.", filepath.Base(filepath.Dir(main)))
		}
	}
}
