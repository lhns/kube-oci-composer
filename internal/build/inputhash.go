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
//
// v2: a Dockerfile that does not live in the context is delivered to buildctl as its own
// `--local dockerfile=` mount rather than as a path inside the context mount, and a build with no
// context gets a synthesised empty one. Both change the invocation, which is exactly what this
// constant exists to track.
const RecipeVersion = 2

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

	// FetcherDigest pins the image that fetches and unpacks the context.
	//
	// Hashed for the same reason as BuilderDigest, and the argument that it need not be is worth
	// answering: every context is digest-addressed, so a correct fetcher has exactly one possible
	// output. That holds for the DOWNLOAD and fails for the UNPACK -- a fixed symlink or wrapper
	// bug changes the tree under an unchanged digest, which is exactly the failure BuilderDigest
	// exists to prevent.
	FetcherDigest string

	// ContextKind is which member of the context union was resolved -- "sourceRef", or "" when the
	// build has no context at all.
	//
	// Hashed, and hashed BEFORE the digest, because a digest alone no longer says what it
	// addresses. It also distinguishes "no context" from "a context whose digest is not yet
	// resolved", which are the same empty string in ContextDigest and very much not the same input.
	ContextKind string

	// ContextDigest is what addresses the context content -- for a Flux source, the artifact's
	// digest, RESOLVED rather than declared. Empty when there is no context; ContextKind is what
	// tells those apart.
	ContextDigest string
	// ContextRevision is what the artifact digest DESCRIBES — "v0.6.8@sha1:b739efb5". Recorded so
	// a built image can be traced back to a revision without pulling it apart, which is the gap
	// ADR 0026's incident was diagnosed through. Not hashed: the digest already identifies the
	// content, and hashing both would rebuild on a repack that changed nothing.
	ContextRevision string
	ContextSubpath  string

	// ContextUnpack is how the fetched archive becomes a tree. Hashed because the same bytes become
	// different trees under different modes, and the tree is what the build sees.
	ContextUnpack string

	// DockerfileKind is "path" or "inline".
	//
	// Hashed for the same reason as ContextKind, and for a sharper one: the two forms carry their
	// meaning in DIFFERENT fields, so without this a path of "" and an inline of "" would hash the
	// same.
	DockerfileKind string

	// Dockerfile is the PATH, and only for the path form. It stays a path there for the reason it
	// always was: the content lives inside the context, which ContextDigest addresses, so an edit
	// to it already moves this hash and nothing has to be fetched to compute one.
	Dockerfile string

	// DockerfileDigest is a sha256 over the Dockerfile's BYTES, for the forms that do not ride
	// inside the context. Empty for the path form.
	//
	// CONTENT, not an identity -- the opposite of SecretIdentities below, and the contrast is
	// deliberate. That field hashes name/resourceVersion because status.inputHash is readable by
	// anyone with get and a hash of a low-entropy secret is an oracle. Neither half holds here: a
	// Dockerfile is not low-entropy and is not meant to be unknowable, and it is precisely the
	// content that decides what gets built.
	DockerfileDigest string

	Target    string
	Network   string
	CacheMode string
	CacheRef  string

	// Attestations records whether BuildKit was asked for an SBOM and provenance.
	//
	// Hashed, deliberately, because it changes WHAT IS PUSHED -- attestations make the output an
	// index rather than a manifest. The pleasant consequence is that enabling them re-runs every
	// build once, visibly, rather than leaving existing objects converged at a digest with no
	// attestations and no record of why.
	Attestations string

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
	writeField(in.FetcherDigest)
	writeField(in.ContextKind)
	writeField(in.ContextDigest)
	writeField(in.ContextSubpath)
	writeField(in.ContextUnpack)
	writeField(in.DockerfileKind)
	writeField(in.Dockerfile)
	writeField(in.DockerfileDigest)
	writeField(in.Target)
	writeField(in.Network)
	writeField(in.CacheMode)
	writeField(in.CacheRef)
	writeField(in.SourceDateEpoch)
	writeField(in.Attestations)

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
