// Package build turns an ImageBuild spec into a build, and decides when one is needed.
//
// Deliberately separate from internal/oci, which assembles layers and executes nothing. ADR 0025
// records that internal/oci contributes nothing to a build.
package build

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// RecipeVersion identifies how this controller turns a spec into a build.
//
// The counterpart of oci.AssemblyVersion: an upgraded controller that invokes BuildKit differently
// must not look at an unchanged input hash and keep serving an artifact from the old invocation.
// BUMP THIS whenever the argv, the exporter attributes, the frontend options or the default
// SOURCE_DATE_EPOCH change. Unlike AssemblyVersion it is not the whole story, because the tool is
// not in this binary -- that is BuilderDigest.
//
// v2: a Dockerfile outside the context gets its own `--local dockerfile=` mount, and a build with
// no context gets a synthesised empty one.
const RecipeVersion = 2

// Inputs is everything that determines a build's output, as far as anything here can determine it.
//
// The qualifier is the difference from oci.InputHash: there the hash is a short-circuit and the
// output digest is the identity; here the hash IS the identity, and two builds with the same
// Inputs may still produce different bytes. See ADR 0025.
type Inputs struct {
	// BuilderDigest pins the BuildKit image, and FrontendDigest the Dockerfile frontend.
	//
	// Hashed because the algorithm is not in this binary -- BuildKit is. Upgrading the builder
	// therefore rebuilds every object in the cluster, which is accepted rather than worked around.
	BuilderDigest  string
	FrontendDigest string

	// FetcherDigest pins the image that fetches and unpacks the context.
	//
	// Hashed for the same reason as BuilderDigest. Digest-addressing the context does not make it
	// redundant: that pins the download, not the unpack, and a fixed symlink or wrapper bug changes
	// the tree under an unchanged digest.
	FetcherDigest string

	// ContextKind is which member of the context union was resolved -- "sourceRef", "fetch",
	// "image", or "" for no context at all.
	//
	// Hashed before the digest, because a digest alone no longer says what it addresses. It also
	// separates "no context" from "a context not yet resolved", which share an empty ContextDigest.
	ContextKind string

	// ContextDigest addresses the context content. Empty when there is no context; ContextKind is
	// what tells those apart.
	ContextDigest string
	// ContextRevision is what the artifact digest describes -- "v0.6.8@sha1:b739efb5". Recorded so
	// a built image traces back to a revision without being pulled apart. Not hashed: the digest
	// already identifies the content, and hashing both would rebuild on a no-op repack.
	ContextRevision string
	ContextSubpath  string

	// ContextStrip is how many leading path components the fetcher removes. Hashed because the same
	// bytes become a different tree at a different depth -- exactly why ContextUnpack is hashed.
	ContextStrip int

	// ContextUnpack is how the fetched archive becomes a tree. Hashed because the same bytes become
	// different trees under different modes, and the tree is what the build sees.
	ContextUnpack string

	// DockerfileKind is "path", "inline" or "configMap".
	//
	// Hashed for the same reason as ContextKind, and a sharper one: the forms carry their meaning
	// in different fields, so without it a path of "" and an inline of "" would hash the same.
	DockerfileKind string

	// Dockerfile is the path, and only for the path form: the content lives inside the context,
	// which ContextDigest already addresses, so nothing has to be fetched to compute a hash.
	Dockerfile string

	// DockerfileDigest is a sha256 over the Dockerfile's bytes, for the forms that do not ride
	// inside the context. Empty for the path form.
	//
	// Content, not an identity -- deliberately the opposite of SecretIdentities below, which hashes
	// name/resourceVersion because status.inputHash is world-readable and a hash of a low-entropy
	// secret is an oracle. A Dockerfile is neither low-entropy nor meant to be unknowable.
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
	fmt.Fprintf(h, "strip=%d;", in.ContextStrip)
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
