package oci

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// epoch is the fixed timestamp stamped on every tar entry and on the image config, so the output
// digest is a pure function of the spec.
var epoch = time.Unix(0, 0).UTC()

// AssemblyVersion identifies the output format produced by Assemble.
//
// It is folded into InputHash. BUMP THIS whenever Assemble's output changes for identical inputs
// (entry ordering, header normalisation, media types, the config), or an upgraded controller will
// keep the old artifact under an unchanged input hash.
//
// THE TOOLCHAIN COUNTS TOO: a Go upgrade that changes compress/flate's output moves every digest
// under an unchanged spec-hash tag. It is the Go the RELEASE IMAGE is built with that matters --
// the Dockerfile's, which CI keeps in step with go.mod (ADR 0057).
const AssemblyVersion = 3

// identity returns what the hash should treat as this entry's content.
func (in LayerInput) identity() string {
	if in.Identity != "" {
		return in.Identity
	}
	return in.Digest
}

// InputHash returns a stable hash of everything that determines the assembled output.
//
// Only fields that affect the result are included. Name, URL and Path are not: two sources of the
// same digest are interchangeable, and Path differs every reconcile. Fields are length-prefixed so
// no combination of values can collide with another.
//
// baseDigest is the spec's base pin, empty for a scratch artifact; it also stands in for the
// platform when none is declared. platforms are those known WITHOUT fetching anything: the
// declared list, or the controller's own when there is no base. Empty when the platform comes
// from the base, which baseDigest already pins.
func InputHash(inputs []LayerInput, cfg Config, baseDigest string, platforms []Platform) string {
	h := sha256.New()
	writeField := func(s string) {
		fmt.Fprintf(h, "%d:", len(s))
		h.Write([]byte(s))
	}

	writeField(fmt.Sprintf("assembly-v%d", AssemblyVersion))

	writeField(baseDigest)
	fmt.Fprintf(h, "platforms=%d;", len(platforms))
	for _, p := range platforms {
		writeField(p.String())
	}

	fmt.Fprintf(h, "layers=%d;", len(inputs))
	for _, in := range inputs {
		writeField(in.identity())
		writeField(string(in.Unpack))
		writeField(in.Subpath)
		fmt.Fprintf(h, "strip=%d;", in.StripComponents)
		writeField(in.Target)
		fmt.Fprintf(h, "u=%d;g=%d;fm=%d;dm=%d;", in.UID, in.GID, in.FileMode, in.DirMode)
		fmt.Fprintf(h, "rm=%d;", len(in.Remove))
		for _, r := range in.Remove {
			writeField(r)
		}
	}

	// Every config field reaches the output digest, so all of them must move the hash.
	fmt.Fprintf(h, "inherit=%t;", cfg.Inherit)
	writeField(cfg.User)
	writeField(cfg.WorkingDir)
	writeField(cfg.StopSignal)

	// Labels are a map, so they need a stable order.
	keys := make([]string, 0, len(cfg.Labels))
	for k := range cfg.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(h, "labels=%d;", len(keys))
	for _, k := range keys {
		writeField(k)
		writeField(cfg.Labels[k])
	}
	for _, group := range [][]string{cfg.Env, cfg.Entrypoint, cfg.Cmd, cfg.ExposedPorts, cfg.Volumes} {
		fmt.Fprintf(h, "n=%d;", len(group))
		for _, v := range group {
			writeField(v)
		}
	}

	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Platform is one os/architecture[/variant] an artifact is built for.
type Platform struct {
	OS           string
	Architecture string
	Variant      string
}

// ParsePlatform reads the "linux/arm64" or "linux/arm/v7" form used in the spec.
func ParsePlatform(s string) (Platform, error) {
	parts := strings.Split(s, "/")
	switch len(parts) {
	case 2:
		return Platform{OS: parts[0], Architecture: parts[1]}, nil
	case 3:
		return Platform{OS: parts[0], Architecture: parts[1], Variant: parts[2]}, nil
	default:
		return Platform{}, fmt.Errorf("%q is not a platform (want os/arch or os/arch/variant)", s)
	}
}

func (p Platform) String() string {
	if p.Variant != "" {
		return p.OS + "/" + p.Architecture + "/" + p.Variant
	}
	return p.OS + "/" + p.Architecture
}

func (p Platform) toV1() v1.Platform {
	return v1.Platform{OS: p.OS, Architecture: p.Architecture, Variant: p.Variant}
}

// RuntimePlatform is the platform a base-less artifact is built for when the spec names none.
//
// This is the ONE input to the output digest that does not come from the spec (ADR 0002): on a
// mixed-architecture cluster, name `platforms` or pin the controller to one architecture.
//
// The OS is always linux, NOT runtime.GOOS, which is where the binary happens to run (possibly
// windows or darwin on a developer machine).
func RuntimePlatform() Platform {
	return Platform{OS: "linux", Architecture: runtime.GOARCH}
}

// platformFor resolves the single platform of a build with no explicit platform list: the base's
// if there is a base, the controller's own otherwise.
func platformFor(base v1.Image) (Platform, error) {
	if base == nil {
		return RuntimePlatform(), nil
	}
	cf, err := base.ConfigFile()
	if err != nil {
		return Platform{}, fmt.Errorf("reading the base config: %w", err)
	}
	plat := RuntimePlatform()
	if cf.OS != "" {
		plat.OS = cf.OS
	}
	if cf.Architecture != "" {
		plat.Architecture = cf.Architecture
	}
	plat.Variant = cf.Variant
	return plat, nil
}

// ErrUnsupportedUnpack is returned for an unpack mode this build does not implement.
//
// Typed so the reconciler maps it to a TERMINAL condition: retrying cannot add a code path. The
// CRD enum rejects typos, so the realistic cause is a CRD newer than the controller.
type ErrUnsupportedUnpack struct {
	Mode string
}

func (e *ErrUnsupportedUnpack) Error() string {
	return fmt.Sprintf("unknown unpack mode %q: this controller does not implement it, "+
		"so the CRD may be newer than the controller", e.Mode)
}

// UnpackMode mirrors the API's Unpack field.
type UnpackMode string

const (
	UnpackNone    UnpackMode = "none"
	UnpackTar     UnpackMode = "tar"
	UnpackTarGz   UnpackMode = "tar.gz"
	UnpackTarXz   UnpackMode = "tar.xz"
	UnpackTarZstd UnpackMode = "tar.zst"
	UnpackTarBz2  UnpackMode = "tar.bz2"
	UnpackGz      UnpackMode = "gz"
	UnpackZip     UnpackMode = "zip"
	UnpackDeb     UnpackMode = "deb"

	// UnpackImage marks a layer whose content is another image's flattened filesystem. Not in the
	// CRD enum and never dispatched on (LayerInput.Image identifies image layers); it only keeps
	// image layers and `unpack: none` fetches distinct in InputHash.
	UnpackImage UnpackMode = "image"
)

// LayerInput is one content contribution.
//
// It is built from the spec first, with Path empty, so InputHash can be computed before anything
// is downloaded. Path is filled in only once a build is known to be needed.
type LayerInput struct {
	// Name of the entry, used in error messages and provenance. Not part of the output.
	Name string
	// Identity names this entry's CONTENT for hashing when Digest names only its transport: a Flux
	// artifact's tarball digest changes when source-controller re-packs, its revision does not.
	// The fetch is still verified against Digest. Empty means Digest identifies the content.
	Identity string
	// URL the content is fetched from. Not part of the output: two URLs serving the same
	// digest are interchangeable by definition.
	URL string
	// Path to the fetched content on local disk. Empty until fetched, and always empty for an
	// image layer, which has no fetched file.
	Path string
	// Image is the source image for an image layer, resolved and pulled by the caller; its
	// flattened filesystem becomes this entry's content. Nil otherwise. Like Path, it is a handle
	// on content that Digest (the manifest digest) already identifies for InputHash.
	Image v1.Image
	// Digest of the fetched content, already verified.
	Digest string
	// Unpack controls how the bytes become layer content.
	Unpack UnpackMode
	// Subpath selects a directory within an unpacked archive. Empty takes the whole archive.
	Subpath string

	// StripComponents is how many leading path components to remove from every entry, applied
	// BEFORE Subpath. Zero leaves the archive's own paths alone.
	StripComponents int
	// Target is the absolute path inside the image.
	Target string
	// Remove lists absolute paths to delete. Mutually exclusive with a content source; an entry
	// with Remove set produces a whiteout-only layer.
	Remove []string
	// UID and GID own the contributed files. Zero is the default and the common case.
	UID, GID int64
	// FileMode and DirMode override the normalised permissions when non-zero.
	FileMode, DirMode int64
}

// Config is the OCI config to stamp on the produced image.
type Config struct {
	// Inherit starts from the base image's config rather than an empty one.
	Inherit bool

	Labels       map[string]string
	Env          []string
	Entrypoint   []string
	Cmd          []string
	User         string
	WorkingDir   string
	ExposedPorts []string
	Volumes      []string
	StopSignal   string
}

// tarEntry is a file destined for the layer, collected before writing so the archive can be
// emitted in a stable order.
type tarEntry struct {
	name string // cleaned, relative, slash-separated
	mode int64
	body []byte
	dir  bool
	link string
}

// Assemble builds an image from the given inputs, in order. Later entries overlay earlier ones.
//
// The result is byte-for-byte reproducible: entries are sorted, timestamps fixed and ownership
// normalised.
//
// workDir holds the assembled layer files, which go-containerregistry reads lazily, so the CALLER
// must remove it only after the image has been consumed. An empty workDir uses the system temp
// directory and leaves the files behind.
func Assemble(base v1.Image, inputs []LayerInput, cfg Config, workDir string) (v1.Image, error) {
	plat, err := platformFor(base)
	if err != nil {
		return nil, err
	}
	return AssembleAs(base, inputs, cfg, plat, workDir)
}

// AssembleAs is Assemble with the platform stated rather than derived, for a spec that names
// exactly one. The output is a single image manifest, not an index — one platform is one image.
func AssembleAs(base v1.Image, inputs []LayerInput, cfg Config, plat Platform, workDir string) (v1.Image, error) {
	layers, err := buildLayers(inputs, workDir)
	if err != nil {
		return nil, err
	}
	return assembleFor(base, layers, inputs, cfg, plat)
}

// AssembleIndex builds one image per platform and returns them as an OCI image index.
//
// The layer tarballs are built ONCE and shared by every child, so the children cannot disagree
// about content that is the same on every platform.
//
// bases maps each platform to its base image, and is nil for a base-less artifact. A platform
// missing from a non-nil map is an error rather than a silent scratch image.
func AssembleIndex(bases map[Platform]v1.Image, inputs []LayerInput, cfg Config,
	platforms []Platform, workDir string) (v1.ImageIndex, error) {
	if len(platforms) == 0 {
		return nil, fmt.Errorf("no platforms given")
	}
	layers, err := buildLayers(inputs, workDir)
	if err != nil {
		return nil, err
	}

	idx := mutate.IndexMediaType(empty.Index, types.OCIImageIndex)
	for _, plat := range platforms {
		var base v1.Image
		if bases != nil {
			b, ok := bases[plat]
			if !ok {
				return nil, fmt.Errorf("no base image for platform %s", plat)
			}
			base = b
		}
		img, err := assembleFor(base, layers, inputs, cfg, plat)
		if err != nil {
			return nil, fmt.Errorf("platform %s: %w", plat, err)
		}
		p := plat.toV1()
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
			Add: img,
			Descriptor: v1.Descriptor{
				// What a kubelet reads to pick a child.
				Platform: &p,
			},
		})
	}
	return idx, nil
}

// buildLayers converts every input into a deterministic layer, once.
//
// The layer files are read lazily; see Assemble on workDir's lifetime.
func buildLayers(inputs []LayerInput, workDir string) ([]v1.Layer, error) {
	layers := make([]v1.Layer, 0, len(inputs))
	for _, in := range inputs {
		layerPath, err := buildLayerTarGz(in, workDir)
		if err != nil {
			return nil, fmt.Errorf("layer %q: %w", in.Name, err)
		}
		layer, err := tarball.LayerFromFile(layerPath, tarball.WithMediaType(types.OCILayer))
		if err != nil {
			return nil, fmt.Errorf("layer %q: reading assembled tar: %w", in.Name, err)
		}
		layers = append(layers, layer)
	}
	return layers, nil
}

// assembleFor stacks the layers on the base and stamps the config for one platform.
func assembleFor(base v1.Image, layers []v1.Layer, inputs []LayerInput, cfg Config, plat Platform) (v1.Image, error) {
	// The base's layers come first and are reused verbatim, keeping their digests and sharing.
	img := empty.Image
	if base != nil {
		img = base
	}
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.OCIConfigJSON)

	for i, layer := range layers {
		var err error
		img, err = mutate.AppendLayers(img, layer)
		if err != nil {
			return nil, fmt.Errorf("layer %d: appending: %w", i, err)
		}
	}

	cf, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	cf = cf.DeepCopy()

	// Inheritance is opt-in: silently acquiring a base's entrypoint would be surprising (ADR 0015).
	if cfg.Inherit {
		if base == nil {
			return nil, fmt.Errorf("config.inherit is set but there is no base to inherit from")
		}
		baseConfig, err := base.ConfigFile()
		if err != nil {
			return nil, fmt.Errorf("reading the base config: %w", err)
		}
		inherited := baseConfig.DeepCopy()
		// RootFS and History describe the assembled image, not the base.
		inherited.RootFS = cf.RootFS
		cf = inherited
	} else if base != nil {
		cf.Config = v1.Config{}
	}

	cf.Created = v1.Time{Time: epoch}
	cf.Author = ""
	// The caller decides the platform: a multi-platform build stamps the same layers once per
	// platform.
	cf.OS = plat.OS
	cf.Architecture = plat.Architecture
	cf.Variant = plat.Variant
	if len(cfg.Labels) > 0 {
		cf.Config.Labels = cfg.Labels
	}
	if len(cfg.Env) > 0 {
		cf.Config.Env = cfg.Env
	}
	if len(cfg.Entrypoint) > 0 {
		cf.Config.Entrypoint = cfg.Entrypoint
	}
	if len(cfg.Cmd) > 0 {
		cf.Config.Cmd = cfg.Cmd
	}
	if cfg.User != "" {
		cf.Config.User = cfg.User
	}
	if cfg.WorkingDir != "" {
		cf.Config.WorkingDir = cfg.WorkingDir
	}
	if cfg.StopSignal != "" {
		cf.Config.StopSignal = cfg.StopSignal
	}
	if len(cfg.ExposedPorts) > 0 {
		cf.Config.ExposedPorts = toSet(cfg.ExposedPorts)
	}
	if len(cfg.Volumes) > 0 {
		cf.Config.Volumes = toSet(cfg.Volumes)
	}
	// History entries carry timestamps; drop them.
	cf.History = nil

	img, err = mutate.ConfigFile(img, cf)
	if err != nil {
		return nil, fmt.Errorf("setting config: %w", err)
	}

	// Provenance last, so it describes the finished manifest.
	return withProvenance(img, base, inputs), nil
}

// toSet converts a list into the map-of-empty-struct shape the OCI config uses.
func toSet(items []string) map[string]struct{} {
	out := make(map[string]struct{}, len(items))
	for _, i := range items {
		out[i] = struct{}{}
	}
	return out
}

// buildLayerTarGz converts one input into a deterministic gzipped tar under workDir and returns
// its path.
func buildLayerTarGz(in LayerInput, workDir string) (string, error) {
	entries, err := collectEntries(in)
	if err != nil {
		return "", err
	}

	// Stable order, so the digest does not depend on iteration order. SliceStable, so equal names
	// keep archive order for the dedupe below.
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	out, err := os.CreateTemp(workDir, "layer-*.tar.gz")
	if err != nil {
		return "", fmt.Errorf("creating layer file: %w", err)
	}
	defer out.Close()

	// gzip.Writer leaves ModTime zero and OS unknown, so the stream is deterministic.
	zw := gzip.NewWriter(out)
	tw := tar.NewWriter(zw)

	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if seen[e.name] {
			// The first occurrence in archive order wins. Overlaying is between layers, not
			// within one archive.
			continue
		}
		seen[e.name] = true

		mode := e.mode
		switch {
		case e.dir && in.DirMode != 0:
			mode = in.DirMode
		case !e.dir && e.link == "" && in.FileMode != 0:
			mode = in.FileMode
		}

		hdr := &tar.Header{
			Name:     e.name,
			Mode:     mode,
			ModTime:  epoch,
			Format:   tar.FormatPAX,
			Uid:      int(in.UID),
			Gid:      int(in.GID),
			Uname:    "",
			Gname:    "",
			Typeflag: tar.TypeReg,
		}
		switch {
		case e.dir:
			hdr.Typeflag = tar.TypeDir
			hdr.Name = e.name + "/"
		case e.link != "":
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = e.link
		default:
			hdr.Size = int64(len(e.body))
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return "", fmt.Errorf("writing header %q: %w", e.name, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write(e.body); err != nil {
				return "", fmt.Errorf("writing %q: %w", e.name, err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		return "", fmt.Errorf("closing tar: %w", err)
	}
	if err := zw.Close(); err != nil {
		return "", fmt.Errorf("closing gzip: %w", err)
	}
	return out.Name(), nil
}

// collectEntries turns one input into the set of files it contributes.
func collectEntries(in LayerInput) ([]tarEntry, error) {
	// A remove entry produces only ".wh." whiteouts, which hide a path without reclaiming space.
	if len(in.Remove) > 0 {
		var out []tarEntry
		for _, p := range in.Remove {
			clean := strings.TrimPrefix(path.Clean("/"+p), "/")
			if clean == "" || clean == "." {
				return nil, fmt.Errorf("cannot remove the root directory")
			}
			dir, base := path.Split(clean)
			out = append(out, tarEntry{name: dir + ".wh." + base, mode: 0o644})
		}
		return out, nil
	}

	target := strings.TrimPrefix(path.Clean("/"+in.Target), "/")

	// An image layer has no fetched file to open.
	if in.Image != nil {
		return extractImage(in.Image, target, in.Subpath, in.StripComponents)
	}

	// Every remaining mode reads the fetched file; *os.File serves both io.Reader and the zip
	// reader's io.ReaderAt.
	f, err := os.Open(in.Path)
	if err != nil {
		return nil, fmt.Errorf("opening content: %w", err)
	}
	defer f.Close()

	// A tar under a codec; see tarCompressions.
	if comp, ok := tarCompressions[in.Unpack]; ok {
		return extractTarball(f, comp, target, in.Subpath, in.StripComponents)
	}

	switch in.Unpack {
	case UnpackNone, "":
		return singleFile(f, in, target, compNone)

	case UnpackGz:
		return singleFile(f, in, target, compGzip)

	case UnpackZip:
		return extractZip(f, target, in.Subpath, in.StripComponents)

	case UnpackDeb:
		return extractDeb(f, target, in.Subpath, in.StripComponents)

	default:
		// The CRD admits a mode this build does not implement.
		return nil, &ErrUnsupportedUnpack{Mode: string(in.Unpack)}
	}
}

// singleFile places one file at the target, decompressing it first when comp says to.
//
// This is `unpack: none` and `unpack: gz`. The file name comes from the spec only: the gzip header
// and the URL are not in InputHash, so deriving it from either would let identical bytes produce
// different layers under one input hash.
func singleFile(f *os.File, in LayerInput, target string, comp compression) ([]tarEntry, error) {
	if target == "" || strings.HasSuffix(in.Target, "/") {
		return nil, fmt.Errorf("target %q must name a file when unpack is %q", in.Target, in.Unpack)
	}
	// Refused rather than ignored, so a spec mistake does not look like it worked. `none`
	// predates this and still ignores it.
	if comp != compNone && in.Subpath != "" {
		return nil, fmt.Errorf("subpath is not valid with unpack %q: there is no archive to select from", in.Unpack)
	}

	r, closeFn, err := decompress(f, comp)
	if err != nil {
		return nil, err
	}
	defer closeFn()

	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading content: %w", err)
	}
	return append(parentDirs(target), tarEntry{name: target, mode: 0o644, body: body}), nil
}

// extractTarball extracts a tar that may be wrapped in a codec.
//
// Deferring the codec cleanup is safe only because extractTar materialises every entry before it
// returns.
func extractTarball(f *os.File, comp compression, target, subpath string, strip int) ([]tarEntry, error) {
	r, closeFn, err := decompress(f, comp)
	if err != nil {
		return nil, err
	}
	defer closeFn()

	return extractTar(tar.NewReader(r), target, subpath, strip)
}
