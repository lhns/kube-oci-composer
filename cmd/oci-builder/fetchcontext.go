package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/lhns/kube-oci-composer/internal/fetchcontext"
	"github.com/lhns/kube-oci-composer/internal/netguard"
)

// runFetchContext is the init container: it puts an ImageBuild's context on disk for buildctl.
//
// Its own flag set, not the controller's: one binary so one image covers both and the fetcher's
// digest is the operator's own, but no shared configuration -- this process has no cluster access.
//
// Flags rather than a serialised plan, so `kubectl describe pod` shows exactly what this build was
// told to fetch. Nothing secret is ever passed here.
func runFetchContext(args []string) {
	fs := flag.NewFlagSet("fetch-context", flag.ExitOnError)
	var opts fetchcontext.Options
	fs.StringVar(&opts.Kind, "kind", "", "Which context member this is: sourceRef, fetch or image.")
	fs.StringVar(&opts.URL, "url", "", "Archive to fetch.")
	fs.StringVar(&opts.Digest, "digest", "", "sha256 the fetched bytes must have. Required.")
	fs.StringVar(&opts.Unpack, "unpack", "tar.gz", "Archive mode: tar or tar.gz.")
	fs.StringVar(&opts.Subpath, "subpath", "", "Directory inside the archive to take as the context.")
	fs.IntVar(&opts.Strip, "strip-components", 0,
		"Leading path components to remove from every entry, before --subpath is applied.")
	fs.StringVar(&opts.Dest, "dest", "", "Where to write the tree.")
	fs.StringVar(&opts.Dockerfile, "dockerfile", "",
		"Path inside the context whose FROM lines must be digest-pinned. Empty skips the check, "+
			"which is what a Dockerfile from outside the context means.")
	tokenFile := fs.String("token-file", "",
		"File holding the bearer token for the controller's context endpoint.")
	_ = fs.Parse(args)

	// Read from a file, never passed as a flag: argv is visible in `kubectl describe pod` and in
	// every process listing inside the pod, and this is a credential.
	if *tokenFile != "" {
		raw, err := os.ReadFile(*tokenFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "reading the context token:", err)
			os.Exit(1)
		}
		opts.Token = strings.TrimSpace(string(raw))
	}

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
// and the rest of the private ranges only under --fetch-deny-private. Same balance as the composer:
// an artifact server on a private address is an ordinary source, and a guard that refuses those is
// a guard people switch off. See ADR 0036.
//
// Enforced in the dialer rather than by inspecting the URL, so a hostname resolving to a blocked
// address, a redirect to one, and a DNS rebind are all caught. None of those is visible in the URL.
func guardedClient(denyPrivate bool) *http.Client {
	return &http.Client{
		Timeout:   2 * time.Minute,
		Transport: &http.Transport{DialContext: netguard.DialGuard{DenyPrivate: denyPrivate}.DialContext},
	}
}
