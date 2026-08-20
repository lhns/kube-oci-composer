// Package build turns an ImageBuild spec into a build, and decides when one is needed.
//
// Deliberately separate from internal/oci. Nothing here assembles a tar or fetches a blob by
// digest, and nothing there executes anything; sharing a package would let a refactor of one
// silently change the other, when the whole point of the second kind is that its promise is
// different. ADR 0025 records that internal/oci contributes nothing to a build, which is itself
// evidence that this sits beside the composer rather than extending it.
package build

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// RecipeVersion identifies how this controller turns a spec into a build.
//
// The exact counterpart of oci.AssemblyVersion, and load-bearing for the same reason: an upgraded
// controller that invokes BuildKit differently must not look at an unchanged input hash and keep
// serving an artifact produced by the old invocation. BUMP THIS whenever the argv, the exporter
// attributes, the frontend options or the default SOURCE_DATE_EPOCH change.
//
// Unlike AssemblyVersion this is NOT the whole story, because the tool is not in this binary. That
// is what BuilderDigest is for.
const RecipeVersion = 1

// Inputs is everything that determines a build's output, as far as anything here can determine it.
//
// The qualifier is the whole difference from oci.InputHash: there the hash is a short-circuit and
// the output digest remains the identity, here the hash IS the identity. Two builds with the same
// Inputs may still produce different bytes. See ADR 0025.
type Inputs struct {
	// BuilderDigest pins the BuildKit image, and FrontendDigest the Dockerfile frontend.
	//
	// Hashed because for this kind the algorithm is not in this binary — BuildKit is. Upgrading
	// the builder therefore rebuilds every object in the cluster, which is accepted rather than
	// worked around. See RecipeVersion above and ADR 0025.
	BuilderDigest  string
	FrontendDigest string

	// ContextDigest is the Flux artifact's digest, RESOLVED rather than declared.
	//
	// The Dockerfile's own content needs no separate hashing: it lives inside the context tarball,
	// which is content-addressed, so a change to it moves this. That is why the hash can be
	// computed without fetching anything.
	ContextDigest string
	// ContextRevision is what the artifact digest DESCRIBES — "v0.6.8@sha1:b739efb5". Recorded so
	// a built image can be traced back to a revision without pulling it apart, which is the gap
	// ADR 0026's incident was diagnosed through. Not hashed: the digest already identifies the
	// content, and hashing both would rebuild on a repack that changed nothing.
	ContextRevision string
	ContextSubpath  string

	Dockerfile string
	Target     string
	Network    string
	CacheMode  string
	CacheRef   string

	// SourceDateEpoch is the timestamp policy, not a wall clock. A clock in the hash would make
	// every reconcile a rebuild.
	SourceDateEpoch string

	Platforms []string
	// Args is a set: ARG order in a spec does not change what the build sees.
	Args map[string]string

	// SecretIdentities are "name/resourceVersion" per referenced Secret — never the value.
	//
	// status.inputHash is readable by anyone with get on the object, and a hash of a low-entropy
	// secret is an oracle. Hashing the resourceVersion means a rotation rebuilds; the cost is that
	// a no-op update to the Secret also rebuilds, which is the right way round.
	SecretIdentities []string
}

// Hash returns a stable summary of everything in Inputs.
//
// Fields are length-prefixed rather than delimiter-joined, for the reason oci.InputHash gives: no
// combination of values can then produce the same byte stream as a different combination.
func (in Inputs) Hash() string {
	h := sha256.New()
	writeField := func(s string) {
		fmt.Fprintf(h, "%d:", len(s))
		h.Write([]byte(s))
	}

	writeField(fmt.Sprintf("recipe-v%d", RecipeVersion))
	writeField(in.BuilderDigest)
	writeField(in.FrontendDigest)
	writeField(in.ContextDigest)
	writeField(in.ContextSubpath)
	writeField(in.Dockerfile)
	writeField(in.Target)
	writeField(in.Network)
	writeField(in.CacheMode)
	writeField(in.CacheRef)
	writeField(in.SourceDateEpoch)

	// Platforms are ordered by the spec and the order reaches the output index, so it is NOT
	// sorted away.
	fmt.Fprintf(h, "platforms=%d;", len(in.Platforms))
	for _, p := range in.Platforms {
		writeField(p)
	}

	names := make([]string, 0, len(in.Args))
	for name := range in.Args {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Fprintf(h, "args=%d;", len(names))
	for _, name := range names {
		writeField(name)
		writeField(in.Args[name])
	}

	ids := append([]string(nil), in.SecretIdentities...)
	sort.Strings(ids)
	fmt.Fprintf(h, "secrets=%d;", len(ids))
	for _, id := range ids {
		writeField(id)
	}

	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
