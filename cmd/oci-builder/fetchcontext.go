package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/lhns/kube-oci-composer/internal/fetchcontext"
	"github.com/lhns/kube-oci-composer/internal/netguard"
)

// runFetchContext is the init container: it puts an ImageBuild's context on disk for buildctl.
//
// Its own flag set, not the controller's. The two share a binary so that one image covers both and
// the fetcher's digest is the operator's own, but they share no configuration -- this process has no
// cluster access and needs none.
//
// Flags rather than a serialised plan, so the pod is self-documenting: `kubectl describe pod` shows
// exactly what this build was told to fetch. Nothing secret is ever passed here.
func runFetchContext(args []string) {
	fs := flag.NewFlagSet("fetch-context", flag.ExitOnError)
	var opts fetchcontext.Options
	fs.StringVar(&opts.Kind, "kind", "", "Which context member this is: sourceRef or fetch.")
	fs.StringVar(&opts.URL, "url", "", "Archive to fetch.")
	fs.StringVar(&opts.Digest, "digest", "", "sha256 the fetched bytes must have. Required.")
	fs.StringVar(&opts.Unpack, "unpack", "tar.gz", "Archive mode: tar or tar.gz.")
	fs.StringVar(&opts.Subpath, "subpath", "", "Directory inside the archive to take as the context.")
	fs.StringVar(&opts.Dest, "dest", "", "Where to write the tree.")
	_ = fs.Parse(args)

	if err := fetchcontext.Run(context.Background(), opts); err != nil {
		fmt.Fprintln(os.Stderr, "fetching the build context:", err)

		// A digest mismatch exits distinguishably, so the controller can report it as a spec
		// problem rather than as "the build failed". Everything else is an ordinary failure.
		var mismatch *fetchcontext.MismatchError
		if errors.As(err, &mismatch) {
			os.Exit(fetchcontext.ExitDigestMismatch)
		}
		os.Exit(1)
	}
}

// guardedClient is the HTTP client the CONTROLLER uses to read a Dockerfile out of a context.
//
// Link-local is refused unconditionally -- that is where every major cloud serves credentials --
// and the rest of the private ranges only under --fetch-deny-private, which is the composer's
// balance and made for the composer's reason: an artifact server on a private address is an
// ordinary source, so a guard that refuses those is a guard people switch off. See ADR 0036.
//
// Enforced in the dialer rather than by inspecting the URL, so a hostname resolving to a blocked
// address, a redirect to one, and a DNS rebind are all caught. None of those is visible in the URL.
func guardedClient(denyPrivate bool) *http.Client {
	return &http.Client{
		Timeout:   2 * time.Minute,
		Transport: &http.Transport{DialContext: netguard.DialGuard{DenyPrivate: denyPrivate}.DialContext},
	}
}
