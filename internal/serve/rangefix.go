package serve

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// blobSizer is the part of the blob handler this needs.
type blobSizer interface {
	Stat(ctx context.Context, repo string, h v1.Hash) (int64, error)
}

// closeOpenEndedRanges rewrites `Range: bytes=N-` into `bytes=N-<size-1>` before the registry
// handler sees it.
//
// The handler is upstream go-containerregistry, which parses the header with
// `fmt.Sscanf(h, "bytes=%d-%d")` and answers 416 BLOB_UNKNOWN when that fails. An open-ended range
// is what containerd sends to RESUME an interrupted layer download, and it is valid per RFC 9110,
// so an interrupted pull currently fails permanently instead of continuing.
//
// Fixed here rather than upstream because the size is only knowable from our blob store, and
// rewriting the header is the smallest change that does not fork the handler. Suffix ranges
// (`bytes=-N`) are left alone: upstream rejects those too, but containerd does not send them, and
// inventing behaviour for a case nothing exercises would be worse than the honest 416.
func closeOpenEndedRanges(next http.Handler, blobs blobSizer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if closed, ok := closedRangeFor(r, blobs); ok {
			r.Header.Set("Range", closed)
		}
		next.ServeHTTP(w, r)
	})
}

// closedRangeFor returns the closed form of this request's open-ended blob range, if it has one.
// Anything it cannot resolve is left untouched for the handler to answer as it already would.
func closedRangeFor(r *http.Request, blobs blobSizer) (string, bool) {
	start, ok := parseOpenEnded(r.Header.Get("Range"))
	if !ok {
		return "", false
	}
	h, repo, ok := blobRequest(r.Method, r.URL.Path)
	if !ok {
		return "", false
	}
	size, err := blobs.Stat(r.Context(), repo, h)
	if err != nil || size <= 0 || start >= size {
		return "", false
	}
	return fmt.Sprintf("bytes=%d-%d", start, size-1), true
}

// parseOpenEnded reports the start offset of `bytes=N-`, and whether the header is that shape.
func parseOpenEnded(header string) (int64, bool) {
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok {
		return 0, false
	}
	// One range only: multi-range is not something containerd sends, and upstream cannot serve it.
	if strings.ContainsRune(spec, ',') {
		return 0, false
	}
	start, end, found := strings.Cut(spec, "-")
	if !found || end != "" || start == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(start, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// blobRequest returns the digest and repository of a blob read, and whether the path is one.
func blobRequest(method, path string) (v1.Hash, string, bool) {
	if method != http.MethodGet && method != http.MethodHead {
		return v1.Hash{}, "", false
	}
	elem := strings.Split(strings.Trim(path, "/"), "/")
	if len(elem) < 4 || elem[0] != "v2" || elem[len(elem)-2] != "blobs" {
		return v1.Hash{}, "", false
	}
	h, err := v1.NewHash(elem[len(elem)-1])
	if err != nil {
		return v1.Hash{}, "", false
	}
	return h, strings.Join(elem[1:len(elem)-2], "/"), true
}
