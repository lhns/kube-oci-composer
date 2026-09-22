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
// Its own flag set: this process has no cluster access. Flags rather than a serialised plan, so
// `kubectl describe pod` shows what was fetched; nothing secret is passed as a flag.
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

	// A file, never a flag: argv is visible in `kubectl describe pod` and process listings.
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

		// A digest mismatch exits distinguishably, as a spec problem.
		var mismatch *fetchcontext.MismatchError
		if errors.As(err, &mismatch) {
			os.Exit(fetchcontext.ExitDigestMismatch)
		}
		os.Exit(1)
	}
}

// guardedClient is the HTTP client the controller uses to read a Dockerfile out of a context.
// Link-local is always refused (cloud metadata credentials); other private ranges only under
// --fetch-deny-private. Enforced in the dialer, so resolved names, redirects and DNS rebinds are
// all caught. ADR 0036.
func guardedClient(denyPrivate bool) *http.Client {
	return &http.Client{
		Timeout:   2 * time.Minute,
		Transport: &http.Transport{DialContext: netguard.DialGuard{DenyPrivate: denyPrivate}.DialContext},
	}
}
