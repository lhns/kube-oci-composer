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

// The two kinds are separate components on purpose (ADR 0004), but they answer the same questions:
// which failures stall, which wait, how a reference is scoped, how a message is recorded. Nothing
// made those answers agree, and each way of disagreeing has already happened:
//
//   - `event` truncated messages in one controller and not the other, so an over-long message was
//     rejected by the API server for one kind and shortened for the other. The difference was
//     invisible because each helper was correct on its own terms.
//   - the same-namespace refusal and the revision check went into both controllers but were tested
//     for one, so the second was claimed rather than verified.
//
// This is unpackparity_test.go's argument applied to the controllers instead of the unpack modes:
// a property held in two hand-maintained places needs something that reads both. These tests are
// deliberately STRUCTURAL — they read the source — because the alternative is standing up two
// controllers and asserting behaviour twice, which is the duplication being guarded against.

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

// TestBothKindsScopeReferencesToTheirOwnNamespace — the tenancy boundary is the same for both, and
// it is enforced in code rather than in CEL because a CRD validation rule cannot read
// metadata.namespace. That means nothing but this checks that BOTH controllers do it.
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
// field on the same type. A pin honoured by one kind and ignored by the other is worse than one
// nobody implements, because the spec looks identical.
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
// internal/reconciler. A local re-implementation is how they drifted last time: two `event`
// helpers, one of which truncated.
func TestNeitherKindKeepsItsOwnCopyOfTheSharedHelpers(t *testing.T) {
	// Names that belong to internal/reconciler now. A package-level func with one of these names
	// in either controller is a copy that can drift.
	shared := map[string]bool{
		"setCondition": true, "removeCondition": true, "truncate": true,
		"terminal": true, "pending": true, "isTerminal": true, "isPending": true,
		"recordHistory": true, "interval": true,
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

// TestBothKindsRecordEventsThroughTheSharedHelper — the API server rejects an over-long event
// outright rather than truncating it, and the messages that get long are the failures worth
// reading. One controller truncated and the other did not.
func TestBothKindsRecordEventsThroughTheSharedHelper(t *testing.T) {
	for file, body := range controllerSources(t) {
		if !strings.Contains(body, "Recorder.Event(") {
			continue
		}
		t.Errorf("%s calls Recorder.Event directly instead of recon.Event, which is what applies "+
			"the length limit", filepath.Base(file))
	}
}

// TestPublishAndPushOfferTheSameKnobs — the two kinds' output blocks are `Publish` (serve) and
// `Push` (external registry). They are different destinations, but the questions an operator asks
// of them are identical: which tags, how many past builds to keep, what happens to a tag that
// already exists. Every one of those had drifted:
//
//   - `Push` had no `history`, so the only kind whose artifacts CANNOT be rebuilt from spec
//     (ADR 0025) was also the only one that could not say how many to retain.
//   - `Push` had no `ref`, so the spec-hash tag pattern documented for ImageComposition could not
//     be used from ImageBuild at all.
//
// Neither was a decision; both were oversights that survived because nothing compared the two
// structs. This is the guard that makes the next one a test failure instead of a bug report.
//
// Fields genuinely specific to a destination are listed as exceptions WITH their reason, so adding
// one is a deliberate act rather than something that happens by not noticing.
func TestPublishAndPushOfferTheSameKnobs(t *testing.T) {
	// Only where the destination itself differs. Anything else belongs on both.
	onlyPublish := map[string]string{
		"expiry": "serving-side retention of content tags; the registry owns this for push",
		"name": "the repository path under the serving host; Push spells this `repository` and " +
			"carries the host with it",
	}
	onlyPush := map[string]string{
		"repository": "the external target, which serving derives from its own host",
		"secretRef":  "registry credentials, which serving does not need",
		"insecure":   "plain HTTP to a registry, meaningless when serving in-process",
	}

	publish := jsonFieldsOf(t, "Publish")
	push := jsonFieldsOf(t, "Push")

	for f := range publish {
		if _, ok := push[f]; !ok {
			if _, allowed := onlyPublish[f]; !allowed {
				t.Errorf("Publish has %q and Push does not. Either add it to Push, or record in "+
					"onlyPush why the destination makes it meaningless — an operator should not "+
					"have to learn which kind supports which knob.", f)
			}
		}
	}
	for f := range push {
		if _, ok := publish[f]; !ok {
			if _, allowed := onlyPush[f]; !allowed {
				t.Errorf("Push has %q and Publish does not. Either add it to Publish, or record "+
					"in onlyPublish why it cannot apply.", f)
			}
		}
	}
}

// jsonFieldsOf returns the JSON names of a struct declared in the API package.
func jsonFieldsOf(t *testing.T, typeName string) map[string]bool {
	t.Helper()

	dir := filepath.Join("..", "..", "api", "v1alpha1")
	// ParseFile over a glob rather than ParseDir: the latter is deprecated because it ignores build
	// tags when grouping files into packages, and here there is no package to assemble anyway --
	// only one type declaration to find.
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("listing %s: %v", dir, err)
	}

	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != typeName {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					t.Fatalf("%s is not a struct", typeName)
				}
				out := map[string]bool{}
				for _, f := range st.Fields.List {
					if f.Tag == nil {
						continue
					}
					name := jsonName(f.Tag.Value)
					if name != "" && name != "-" {
						out[name] = true
					}
				}
				return out
			}
		}
	}
	t.Fatalf("type %s not found in %s", typeName, dir)
	return nil
}

// jsonName pulls the field name out of a struct tag literal, dropping ",omitempty" and friends.
func jsonName(tag string) string {
	const key = `json:"`
	i := strings.Index(tag, key)
	if i < 0 {
		return ""
	}
	rest := tag[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	name, _, _ := strings.Cut(rest[:j], ",")
	return name
}
