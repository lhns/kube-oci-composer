// Package source resolves layer entries that are not plain URLs into local content plus a digest.
//
// Both kinds here are content-addressed by the cluster rather than by a human: a Flux source
// publishes an artifact digest, and a ConfigMap's content can be hashed directly. The controller
// resolves the digest instead of the spec declaring it, which keeps the guarantee in ADR 0002
// intact — output is a pure function of *resolved* inputs — while not asking anyone to paste a
// digest for content they are editing in the same commit.
package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Resolved is content ready to become a layer.
type Resolved struct {
	// Path to a local file holding the content.
	Path string
	// Digest of that file.
	Digest string
	// Unpack mode the content requires.
	Unpack string
	// Empty reports that the source legitimately contributed nothing.
	Empty bool
}

// ErrNotFound signals a referenced object that does not exist.
type ErrNotFound struct{ What string }

func (e *ErrNotFound) Error() string { return e.What + " not found" }

// epoch matches the assembly package's fixed timestamp. Content synthesised here goes through the
// same normalisation as everything else, or the digest would vary run to run and the whole
// short-circuit would stop working.
var epoch = time.Unix(0, 0).UTC()

// ConfigMap turns a ConfigMap's entries into a deterministic tar.
//
// Each key becomes one file. ConfigMap keys cannot contain "/", so this deliberately produces a
// flat directory rather than pretending otherwise — anything needing nested paths wants a
// sourceRef.
func ConfigMap(ctx context.Context, c client.Client, namespace, name string, optional bool, workDir string) (Resolved, error) {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := c.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			if optional {
				return Resolved{Empty: true}, nil
			}
			return Resolved{}, &ErrNotFound{What: fmt.Sprintf("ConfigMap %s", key)}
		}
		return Resolved{}, fmt.Errorf("reading ConfigMap %s: %w", key, err)
	}

	// Both maps, merged and sorted. Iteration order over a Go map is randomised, so without the
	// sort the produced tar — and therefore the artifact digest — would differ between reconciles
	// of identical content.
	entries := make(map[string][]byte, len(cm.Data)+len(cm.BinaryData))
	for k, v := range cm.Data {
		entries[k] = []byte(v)
	}
	for k, v := range cm.BinaryData {
		entries[k] = v
	}
	if len(entries) == 0 {
		return Resolved{Empty: true}, nil
	}

	names := make([]string, 0, len(entries))
	for k := range entries {
		if strings.ContainsAny(k, `/\`) {
			// Kubernetes should reject these already; refusing rather than sanitising means a
			// surprising key never silently lands somewhere unexpected in the image.
			return Resolved{}, fmt.Errorf("ConfigMap %s: key %q contains a path separator", key, k)
		}
		names = append(names, k)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, n := range names {
		body := entries[n]
		if err := tw.WriteHeader(&tar.Header{
			Name:     n,
			Mode:     0o644,
			Size:     int64(len(body)),
			ModTime:  epoch,
			Format:   tar.FormatPAX,
			Typeflag: tar.TypeReg,
		}); err != nil {
			return Resolved{}, fmt.Errorf("writing entry %q: %w", n, err)
		}
		if _, err := tw.Write(body); err != nil {
			return Resolved{}, fmt.Errorf("writing %q: %w", n, err)
		}
	}
	if err := tw.Close(); err != nil {
		return Resolved{}, fmt.Errorf("closing tar: %w", err)
	}
	if err := zw.Close(); err != nil {
		return Resolved{}, fmt.Errorf("closing gzip: %w", err)
	}

	payload := buf.Bytes()
	sum := sha256.Sum256(payload)

	f, err := os.CreateTemp(workDir, "configmap-*.tar.gz")
	if err != nil {
		return Resolved{}, fmt.Errorf("creating temp file: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(payload); err != nil {
		return Resolved{}, fmt.Errorf("writing temp file: %w", err)
	}

	return Resolved{
		Path:   f.Name(),
		Digest: "sha256:" + hex.EncodeToString(sum[:]),
		Unpack: "tar.gz",
	}, nil
}
