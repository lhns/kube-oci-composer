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
const AssemblyVersion = 1

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
func InputHash(inputs []LayerInput, cfg Config) string {
	h := sha256.New()
	writeField := func(s string) {
		fmt.Fprintf(h, "%d:", len(s))
		h.Write([]byte(s))
	}

	writeField(fmt.Sprintf("assembly-v%d", AssemblyVersion))

	fmt.Fprintf(h, "layers=%d;", len(inputs))
	for _, in := range inputs {
		writeField(in.Digest)
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

// UnpackMode mirrors the API's Unpack field.
type UnpackMode string

const (
	UnpackNone  UnpackMode = "none"
	UnpackTar   UnpackMode = "tar"
	UnpackTarGz UnpackMode = "tar.gz"
)

// LayerInput is one content contribution.
//
// It is built from the spec first, with Path empty, so InputHash can be computed before anything
// is downloaded. Path is filled in only once a build is known to be needed.
type LayerInput struct {
	// Name of the entry, used in error messages and provenance. Not part of the output.
	Name string
	// URL the content is fetched from. Not part of the output: two URLs serving the same
	// digest are interchangeable by definition.
	URL string
	// Path to the fetched content on local disk. Empty until fetched.
	Path string
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
	// The base's layers come first and are reused verbatim: they are already content-addressed,
	// so repacking them would change their digests, break sharing with anything else on the same
	// base, and re-upload content the registry already holds.
	img := empty.Image
	if base != nil {
		img = base
	}
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.OCIConfigJSON)

	for _, in := range inputs {
		layerPath, err := buildLayerTarGz(in, workDir)
		if err != nil {
			return nil, fmt.Errorf("layer %q: %w", in.Name, err)
		}
		// The layer file must outlive this function: go-containerregistry reads it lazily when
		// the image is written. The caller removes workDir once the image has been consumed.
		layer, err := tarball.LayerFromFile(layerPath, tarball.WithMediaType(types.OCILayer))
		if err != nil {
			return nil, fmt.Errorf("layer %q: reading assembled tar: %w", in.Name, err)
		}
		img, err = mutate.AppendLayers(img, layer)
		if err != nil {
			return nil, fmt.Errorf("layer %q: appending: %w", in.Name, err)
		}
	}

	cf, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	cf = cf.DeepCopy()

	// Inheritance is opt-in. An artifact that is only ever mounted should have an empty config,
	// and silently acquiring a base's entrypoint would be surprising. See ADR 0015.
	os, arch := "linux", "amd64"
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
		// The platform comes from the base too. Claiming linux/amd64 over an arm64 base produces
		// an image the kubelet refuses to run, for a reason that points nowhere useful.
		if baseConfig.OS != "" {
			os = baseConfig.OS
		}
		if baseConfig.Architecture != "" {
			arch = baseConfig.Architecture
		}
	} else if base != nil {
		// Not inheriting, but the platform must still describe the layers underneath.
		if baseConfig, err := base.ConfigFile(); err == nil {
			if baseConfig.OS != "" {
				os = baseConfig.OS
			}
			if baseConfig.Architecture != "" {
				arch = baseConfig.Architecture
			}
		}
		cf.Config = v1.Config{}
	}

	cf.Created = v1.Time{Time: epoch}
	cf.Author = ""
	cf.OS = os
	cf.Architecture = arch
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
	return img, nil
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
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

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
			continue // last write wins is handled by ordering; skip exact duplicates
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

	switch in.Unpack {
	case UnpackNone, "":
		body, err := os.ReadFile(in.Path)
		if err != nil {
			return nil, fmt.Errorf("reading content: %w", err)
		}
		name := target
		if name == "" || strings.HasSuffix(in.Target, "/") {
			return nil, fmt.Errorf("target %q must name a file when unpack is none", in.Target)
		}
		return append(parentDirs(name), tarEntry{name: name, mode: 0o644, body: body}), nil

	case UnpackTar, UnpackTarGz:
		f, err := os.Open(in.Path)
		if err != nil {
			return nil, fmt.Errorf("opening content: %w", err)
		}
		defer f.Close()

		var r io.Reader = f
		if in.Unpack == UnpackTarGz {
			zr, err := gzip.NewReader(f)
			if err != nil {
				return nil, fmt.Errorf("reading gzip: %w", err)
			}
			defer zr.Close()
			r = zr
		}
		return extractTar(tar.NewReader(r), target, in.Subpath)

	default:
		return nil, fmt.Errorf("unknown unpack mode %q", in.Unpack)
	}
}

// extractTar reads an archive and rebases its entries under target.
//
// When subpath is set, only entries beneath it are taken, and the prefix is stripped so the
// selected directory's contents land at target rather than the directory itself.
func extractTar(tr *tar.Reader, target, subpath string) ([]tarEntry, error) {
	var entries []tarEntry
	dirs := make(map[string]bool)
	prefix := strings.Trim(path.Clean("/"+subpath), "/")
	if prefix == "." {
		prefix = ""
	}
	var matched bool

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading tar: %w", err)
		}

		clean := path.Clean(hdr.Name)
		// Refuse traversal outright rather than sanitising it. A tarball trying to escape its
		// target is not something to quietly correct.
		if clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) && target != "" {
			clean = strings.TrimPrefix(clean, "/")
			if strings.HasPrefix(clean, "../") {
				return nil, fmt.Errorf("archive entry %q escapes the target directory", hdr.Name)
			}
		}
		clean = strings.TrimPrefix(clean, "/")
		if clean == "." || clean == "" {
			continue
		}

		if prefix != "" {
			switch {
			case clean == prefix:
				matched = true
				continue // the directory itself; its contents are what we want
			case strings.HasPrefix(clean, prefix+"/"):
				matched = true
				clean = strings.TrimPrefix(clean, prefix+"/")
			default:
				continue
			}
		}

		name := clean
		if target != "" {
			name = path.Join(target, clean)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if !dirs[name] {
				dirs[name] = true
				entries = append(entries, tarEntry{name: name, mode: 0o755, dir: true})
			}
		case tar.TypeReg:
			body, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("reading %q: %w", hdr.Name, err)
			}
			for _, d := range parentDirs(name) {
				if !dirs[d.name] {
					dirs[d.name] = true
					entries = append(entries, d)
				}
			}
			entries = append(entries, tarEntry{name: name, mode: normaliseMode(hdr.Mode), body: body})
		case tar.TypeSymlink:
			entries = append(entries, tarEntry{name: name, mode: 0o777, link: hdr.Linkname})
		default:
			// Devices, fifos and hard links have no place in an artifact layer.
			continue
		}
	}

	// A subpath that matched nothing is a silent empty layer, and the workload then starts with
	// files missing for no visible reason. Far better to stall on a typo.
	if prefix != "" && !matched {
		return nil, fmt.Errorf("path %q is not present in the archive", subpath)
	}
	return entries, nil
}

// normaliseMode keeps the executable bit and discards the rest, so that permissions from
// whoever built the upstream archive cannot vary the digest in surprising ways.
func normaliseMode(mode int64) int64 {
	if mode&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

// parentDirs returns the directory entries leading to name.
func parentDirs(name string) []tarEntry {
	var out []tarEntry
	dir := path.Dir(name)
	if dir == "." || dir == "/" || dir == "" {
		return nil
	}
	parts := strings.Split(dir, "/")
	for i := range parts {
		out = append(out, tarEntry{name: strings.Join(parts[:i+1], "/"), mode: 0o755, dir: true})
	}
	return out
}
