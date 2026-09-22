// Package build turns an ImageBuild spec into a build, and decides when one is needed.
//
// Separate from internal/oci, which assembles layers and executes nothing (ADR 0025).
package build

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// RecipeVersion identifies how this controller turns a spec into a build: the counterpart of
// oci.AssemblyVersion. Bump it whenever the argv, exporter attributes, frontend options or default
// SOURCE_DATE_EPOCH change. BuildKit itself is covered by BuilderDigest.
//
// v2: a Dockerfile outside the context gets its own `--local dockerfile=` mount, and a build with
// no context gets a synthesised empty one.
const RecipeVersion = 2

// Inputs is everything here that determines a build's output. Unlike oci.InputHash, this hash is
// the identity: the same Inputs may still produce different bytes. ADR 0025.
type Inputs struct {
	// BuilderDigest pins the BuildKit image, and FrontendDigest the Dockerfile frontend. Upgrading
	// either rebuilds everything, by design.
	BuilderDigest  string
	FrontendDigest string

	// FetcherDigest pins the image that fetches and unpacks the context: the context digest pins
	// the download, not the unpack.
	FetcherDigest string

	// ContextKind is which member of the context union was resolved: "sourceRef", "fetch", "image",
	// or "" for none. It says what ContextDigest addresses, and separates no context from an
	// unresolved one.
	ContextKind string

	// ContextDigest addresses the context content. Empty when there is no context.
	ContextDigest string
	// ContextRevision (e.g. "v0.6.8@sha1:b739efb5") is recorded for provenance, not hashed: the
	// digest already identifies the content, and a no-op repack must not rebuild.
	ContextRevision string
	ContextSubpath  string

	// ContextStrip is how many leading path components the fetcher removes. Hashed: a different
	// depth is a different tree.
	ContextStrip int

	// ContextUnpack is how the fetched archive becomes a tree. Hashed for the same reason.
	ContextUnpack string

	// DockerfileKind is "path", "inline" or "configMap"; without it an empty path and an empty
	// inline would hash the same.
	DockerfileKind string

	// Dockerfile is the path, for the path form only; its content is covered by ContextDigest.
	Dockerfile string

	// DockerfileDigest is a sha256 over the Dockerfile's bytes for the forms outside the context.
	// Content is fine to hash here, unlike SecretIdentities: a Dockerfile is not a secret.
	DockerfileDigest string

	Target    string
	Network   string
	CacheMode string
	CacheRef  string

	// Attestations records whether BuildKit was asked for an SBOM and provenance. Hashed because it
	// changes what is pushed (an index rather than a manifest).
	Attestations string

	// SourceDateEpoch is the timestamp policy, not a wall clock.
	SourceDateEpoch string

	Platforms []string
	// Args is a set: ARG order in a spec does not change what the build sees.
	Args map[string]string

	// SecretIdentities are "name/resourceVersion" per referenced Secret, never the value:
	// status.inputHash is readable by anyone with get. A rotation rebuilds.
	SecretIdentities []string
}

// Hash returns a stable summary of everything in Inputs. Fields are length-prefixed so no two
// combinations of values produce the same byte stream.
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

	// Platform order reaches the output index, so it is not sorted.
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
