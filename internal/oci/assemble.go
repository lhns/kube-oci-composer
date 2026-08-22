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

// epoch is the fixed timestamp stamped on every tar entry and on the image config.
//
// Determinism is the property the whole design rests on: the output digest must be a pure
// function of the spec. Real timestamps would make two assemblies of identical inputs produce
// different digests, which would break idempotence, make provenance meaningless, and turn every
// reconcile into a needless push. 1970-01-01 is used rather than "now" for exactly that reason.
var epoch = time.Unix(0, 0).UTC()

// AssemblyVersion identifies the output format produced by Assemble.
//
// It is folded into InputHash, which is what lets the reconciler skip a build whose inputs have
// not changed. BUMP THIS whenever Assemble's output changes for identical inputs — entry
// ordering, header normalisation, media types, the config it stamps. Forgetting to means an
// upgraded controller looks at an artifact built by the old algorithm, sees an unchanged input
// hash, and keeps serving it forever.
const AssemblyVersion = 2

// identity returns what the hash should treat as this entry's content.
func (in LayerInput) identity() string {
	if in.Identity != "" {
		return in.Identity
	}
	return in.Digest
}

// InputHash returns a stable hash of everything that determines the assembled output.
//
// Only the fields that actually affect the result are included: the ordered layer digests, their
// unpack modes and targets, the config, and AssemblyVersion. LayerInput.Name and .Path are
// excluded deliberately — the name appears only in error messages, and the path is a temporary
// location that differs on every reconcile. Including either would defeat the whole point by
// producing a different hash for identical content.
//
// Fields are length-prefixed rather than delimiter-joined so that no combination of targets or
// label values can produce the same byte stream as a different combination.
// baseDigest and platforms cover everything outside the layers that reaches the output.
//
// baseDigest is the spec's base pin, empty for a scratch artifact. It was previously ABSENT from
// this hash, which meant repointing spec.base.digest left the hash unchanged, the cheap path
// short-circuited, and the new base was silently never built. It also stands in for the platform
// when none is declared, since an unset list resolves to the base's platform.
//
// platforms are the platforms that could be determined WITHOUT fetching anything: the declared
// list if the spec has one, or the controller's own when there is no base and nothing declared.
// It is deliberately empty when the platform comes from the base — baseDigest already pins that,
// and fetching the base to compute a hash would defeat the purpose of having one.
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
		writeField(in.Target)
		fmt.Fprintf(h, "u=%d;g=%d;fm=%d;dm=%d;", in.UID, in.GID, in.FileMode, in.DirMode)
		fmt.Fprintf(h, "rm=%d;", len(in.Remove))
		for _, r := range in.Remove {
			writeField(r)
		}
	}

	// Every config field lands in the image config and therefore in the output digest, so all of
	// them must move the hash.
	fmt.Fprintf(h, "inherit=%t;", cfg.Inherit)
	writeField(cfg.User)
	writeField(cfg.WorkingDir)
	writeField(cfg.StopSignal)

	// Labels are a map, so they need a stable order. Everything else is already a slice and
	// carries its own.
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
// This is the ONE input to the output digest that does not come from the spec. ADR 0002 records
// why the exception is taken and what it costs: on a single-architecture cluster it is exactly the
// value that was previously hardcoded, so nothing changes; on a mixed-architecture cluster the same
// spec can produce different content depending on which node the leader is on, and the answers are
// to name `platforms` in the spec or to pin the controller to one architecture.
//
// The OS is linux, NOT runtime.GOOS. GOOS is the platform the controller BINARY was built for,
// which on a developer machine is windows or darwin — and stamping an artifact os=windows would
// produce something no kubelet will mount, from a spec that says nothing about Windows. Every
// artifact this controller produces is a linux container image; only the architecture is genuinely
// in question, and that is what GOARCH answers.
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
// Typed, like ErrDigestMismatch, so the reconciler can map it to a TERMINAL condition: retrying
// cannot add a code path to a running binary. Untyped it was an ordinary error, so the object sat
// Ready=False and requeued with backoff indefinitely without ever saying why.
//
// The realistic cause is version skew rather than a typo, since the CRD's enum rejects anything
// else at admission. The chart ships CRDs under crds/, which Helm installs but never upgrades, so
// a schema newer than its controller is an ordinary situation.
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

	// UnpackImage marks a layer whose content is another image's flattened filesystem.
	//
	// Not a member of the CRD's unpack enum and never dispatched on: an image layer is recognised
	// by LayerInput.Image being set, and there is nothing to unpack because nothing was fetched.
	// It exists so the mode field discriminates image layers inside InputHash. Without it, an
	// image layer and an `unpack: none` fetch would hash identically whenever their digests
	// matched — which needs a preimage attack rather than an accident, but costs nothing to rule
	// out given the field is hashed anyway.
	UnpackImage UnpackMode = "image"
)

// LayerInput is one content contribution.
//
// It is built from the spec first, with Path empty, so InputHash can be computed before anything
// is downloaded. Path is filled in only once a build is known to be needed.
type LayerInput struct {
	// Name of the entry, used in error messages and provenance. Not part of the output.
	Name string
	// Identity is what names this entry's CONTENT for hashing, when Digest names its transport
	// instead. A Flux artifact is the case: source-controller re-packs on restart, so the tarball's
	// digest changes while the revision it describes does not, and hashing the digest rebuilt every
	// composition for bytes that were identical. Digest is still what the fetch is verified
	// against; only the hash reads this. Empty means Digest identifies the content, which is true
	// for everything else.
	Identity string
	// URL the content is fetched from. Not part of the output: two URLs serving the same
	// digest are interchangeable by definition.
	URL string
	// Path to the fetched content on local disk. Empty until fetched, and always empty for an
	// image layer, which has no fetched file.
	Path string
	// Image is the source image for an image layer, resolved and pulled by the caller. Its
	// flattened filesystem becomes this entry's content. Nil for every other kind of entry.
	//
	// Not part of the output by itself: Digest carries the image's manifest digest, and that is
	// what reaches InputHash. Holding the v1.Image here is the same arrangement Path uses — the
	// resolved handle for content the hash already identifies.
	Image v1.Image
	// Digest of the fetched content, already verified.
	Digest string
	// Unpack controls how the bytes become layer content.
	Unpack UnpackMode
	// Subpath selects a directory within an unpacked archive. Empty takes the whole archive.
	// Used by sourceRef layers, where the artifact is a whole repository and usually only one
	// directory of it belongs in the image.
	Subpath string
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
// The result is byte-for-byte reproducible: entries are sorted, timestamps are fixed, and
// ownership is normalised. Two calls with the same inputs produce the same digest, which is what
// lets the reconciler skip work by comparing digests instead of rebuilding.
//
// workDir holds the assembled layer files. They must outlive this call — go-containerregistry
// reads them lazily when the image is written — so the CALLER owns the directory and must remove
// it only after the image has been consumed. An empty workDir uses the system temp directory,
// which leaves the files behind; pass a real directory in any long-running process.
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
// The layer tarballs are built ONCE and shared by every child. That is not just an optimisation:
// composed content is the same bytes on every platform, so rebuilding it per platform would spend
// real time producing identical layers, and any non-determinism in that path would show up as
// children that disagree about content they are supposed to share.
//
// bases maps each platform to the child of the base index selected for it, and is nil for a
// base-less artifact. A platform with no entry is an error rather than a silent scratch image —
// see resolveBase, which is where that selection happens.
//
// Determinism holds exactly as it does for a single image: the children are assembled from the
// same layers in the given platform order, so two calls with the same arguments produce the same
// index digest.
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
				// The descriptor is what a kubelet reads to pick a child. Getting it wrong
				// produces a pull that fails with "no matching manifest", pointing at the
				// workload rather than at the composition that caused it.
				Platform: &p,
			},
		})
	}
	return idx, nil
}

// buildLayers converts every input into a deterministic layer, once.
//
// The layer files must outlive this call: go-containerregistry reads them lazily when the image is
// written, so the CALLER owns workDir and removes it only after the image has been consumed.
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
	// The base's layers come first and are reused verbatim: they are already content-addressed,
	// so repacking them would change their digests, break sharing with anything else on the same
	// base, and re-upload content the registry already holds.
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

	// Inheritance is opt-in. An artifact that is only ever mounted should have an empty config,
	// and silently acquiring a base's entrypoint would be surprising. See ADR 0015.
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
	// The platform is decided by the caller, not derived here — for a multi-platform build the
	// same layers are stamped once per platform, so this function cannot be the one that knows.
	// Claiming linux/amd64 over an arm64 base produces an image the kubelet refuses to run, for a
	// reason that points nowhere useful, which is why platformFor exists rather than a default.
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
	// History entries carry timestamps; drop them rather than stamp them, so nothing
	// non-deterministic leaks into the config digest.
	cf.History = nil

	img, err = mutate.ConfigFile(img, cf)
	if err != nil {
		return nil, fmt.Errorf("setting config: %w", err)
	}

	// Provenance last, so it describes the finished manifest. See provenance.go for why this is
	// annotations rather than config labels, and why nothing here is time-dependent.
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

	// Stable order. Without this the digest would depend on filesystem or archive iteration
	// order, which is exactly the kind of incidental variation determinism must exclude.
	//
	// SliceStable, not Slice: equal names must break ties on archive order, which is a property of
	// the input, rather than on how the sort happened to partition them. See the dedupe below.
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	out, err := os.CreateTemp(workDir, "layer-*.tar.gz")
	if err != nil {
		return "", fmt.Errorf("creating layer file: %w", err)
	}
	defer out.Close()

	// gzip.Writer leaves ModTime zero and OS unknown unless told otherwise, so the compressed
	// stream is itself deterministic.
	zw := gzip.NewWriter(out)
	tw := tar.NewWriter(zw)

	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if seen[e.name] {
			// First occurrence wins, which after the stable sort above means the one the archive
			// listed first. Overlaying BETWEEN layers is what the ordered layers list is for; two
			// entries with one name inside a single archive is an ambiguity, not an overlay.
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
	// A remove entry produces whiteouts and nothing else. OCI expresses deletion as a ".wh."
	// sibling, so the bytes remain in the layer below and this hides a path rather than
	// reclaiming its space.
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

	// An image contributes a filesystem rather than a fetched file, so it returns before the open
	// below — there is no Path to open. Checked here rather than as a switch arm for that reason.
	if in.Image != nil {
		return extractImage(in.Image, target, in.Subpath)
	}

	// Every mode past this point reads the fetched file, so it is opened once here rather than in
	// each arm. Every extractor takes it: the content is always streamed to disk before it gets
	// here, so an *os.File is both the io.Reader the tar and deb readers want and the io.ReaderAt
	// the zip reader needs.
	f, err := os.Open(in.Path)
	if err != nil {
		return nil, fmt.Errorf("opening content: %w", err)
	}
	defer f.Close()

	// A tar under a codec. Looked up rather than listed as case labels, so the set of modes and
	// their codecs cannot disagree — see tarCompressions.
	if comp, ok := tarCompressions[in.Unpack]; ok {
		return extractTarball(f, comp, target, in.Subpath)
	}

	switch in.Unpack {
	case UnpackNone, "":
		return singleFile(f, in, target, compNone)

	case UnpackGz:
		return singleFile(f, in, target, compGzip)

	case UnpackZip:
		return extractZip(f, target, in.Subpath)

	case UnpackDeb:
		return extractDeb(f, target, in.Subpath)

	default:
		// Reached when the CRD admits a mode this build does not implement. Typed so the reconciler
		// reports it as terminal instead of retrying a mode that will never appear.
		return nil, &ErrUnsupportedUnpack{Mode: string(in.Unpack)}
	}
}

// singleFile places one file at the target, decompressing it first when comp says to.
//
// This is `unpack: none` and `unpack: gz` — they are one procedure differing only by a codec, so a
// third single-file mode is one more call rather than another copy of this.
//
// The name in the image comes from the spec and nowhere else. gzip can record an original filename
// in its header and the URL usually ends in one, but both are excluded from InputHash, so deriving
// the name from either would let two mirrors serving identical bytes produce different layers under
// one input hash — after which the reconciler serves whichever was built first, forever.
func singleFile(f *os.File, in LayerInput, target string, comp compression) ([]tarEntry, error) {
	if target == "" || strings.HasSuffix(in.Target, "/") {
		return nil, fmt.Errorf("target %q must name a file when unpack is %q", in.Target, in.Unpack)
	}
	// Refused rather than ignored, because there is no archive to select from and silence would
	// leave a spec mistake looking like it worked. `none` predates this and still ignores it.
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
// Deferring the codec cleanup here is safe because extractTar materialises every entry before it
// returns; a change that made it return a lazy reader would have to move this.
func extractTarball(f *os.File, comp compression, target, subpath string) ([]tarEntry, error) {
	r, closeFn, err := decompress(f, comp)
	if err != nil {
		return nil, err
	}
	defer closeFn()

	return extractTar(tar.NewReader(r), target, subpath)
}
