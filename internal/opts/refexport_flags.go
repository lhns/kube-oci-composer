package opts

import (
	"flag"

	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// ExportFlags registers push.writeRefTo's operator settings on a FlagSet.
//
// Shared by both binaries because the feature is identical on both kinds, and four flag
// descriptions maintained twice would drift -- the way the ConfigMap grant did.
type ExportFlags struct {
	namespaces         string
	labels             string
	allowedLabels      string
	allowedAnnotations string
}

// Register declares the flags. Call before flag.Parse.
func (f *ExportFlags) Register(fs *flag.FlagSet) {
	fs.StringVar(&f.namespaces, "ref-export-namespaces", "",
		"comma-separated namespaces push.writeRefTo may write a ConfigMap in, BESIDES the object's "+
			"own, which is always permitted. Empty refuses every foreign target: a substitution "+
			"source in the namespace that parameterises a cluster is a privilege to grant "+
			"deliberately. This flag is the boundary, not RBAC -- see ADR 0056.")
	fs.StringVar(&f.labels, "ref-export-labels", "",
		"key=value labels added to every ConfigMap push.writeRefTo generates, so whatever watches "+
			"substitution sources notices it change. For Flux: "+
			"reconcile.fluxcd.io/watch=Enabled. Empty adds none.")
	fs.StringVar(&f.allowedLabels, "ref-export-allowed-labels", "",
		"comma-separated label keys an object may set on its exported ConfigMap, exact or with a "+
			"trailing * . Empty permits none. These land on an object in a namespace the object "+
			"may not otherwise touch, so an unpermitted key is refused rather than dropped.")
	fs.StringVar(&f.allowedAnnotations, "ref-export-allowed-annotations", "",
		"comma-separated annotation keys an object may set on its exported ConfigMap, exact or "+
			"with a trailing * . Empty permits none.")
}

// Options is what the flags amount to, for the reconciler.
func (f *ExportFlags) Options() recon.ExportOptions {
	return recon.ExportOptions{
		Namespaces:         SplitList(f.namespaces),
		WatchLabels:        recon.ParseLabels(f.labels),
		AllowedLabels:      SplitList(f.allowedLabels),
		AllowedAnnotations: SplitList(f.allowedAnnotations),
	}
}
