package reconciler

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// Published is what a registry currently holds for a target: what each tag resolves to, and
// whether the digest recorded in status is still present.
//
// Gathered once because two decisions depend on it -- whether there is anything to do, and
// whether doing it would remean a tag -- and both must agree.
type Published struct {
	// Tags maps tag -> the digest it resolves to. Absent means the tag does not exist.
	Tags map[string]string
	// Wanted is how many tags were asked for, so a missing one is distinguishable from a wrong one.
	Wanted int
	// Digest is the recorded digest, present iff it still resolves.
	Digest string
}

// Matches reports whether the given digest is fully published: present by digest, and every
// requested tag already pointing at it. With no tags that is just "the content is there".
func (p Published) Matches(digest string) bool {
	if digest == "" || p.Digest != digest {
		return false
	}
	for _, cur := range p.Tags {
		if cur != digest {
			return false
		}
	}
	return len(p.Tags) == p.Wanted
}

// Conflicts returns the first tag that exists and resolves to something other than digest, with
// what it currently holds. Empty tag means there is no conflict.
//
// Returned rather than acted on: only the caller knows which tag policy applies.
func (p Published) Conflicts(tags []string, digest string) (tag, current string) {
	// Ranged over the slice, not the map, so the reported tag is deterministic.
	for _, t := range tags {
		if cur, ok := p.Tags[t]; ok && cur != digest {
			return t, cur
		}
	}
	return "", ""
}

// ResolvePublished asks the registry what repo's tags currently resolve to, and whether prev's
// digest is still there.
//
// A HEAD failure is deliberately not an error: usually the reference does not exist yet, or the
// store was emptied by a restart.
func ResolvePublished(
	repo string, tags []string, prev *ociv1alpha1.ArtifactStatus,
	refOpts []name.Option, opts []remote.Option,
) (Published, error) {
	state := Published{Tags: make(map[string]string, len(tags)), Wanted: len(tags)}

	for _, tag := range tags {
		ref, err := name.ParseReference(fmt.Sprintf("%s:%s", repo, tag), refOpts...)
		if err != nil {
			return Published{}, Terminal("invalid reference %s:%s: %v", repo, tag, err)
		}
		if desc, err := remote.Head(ref, opts...); err == nil {
			state.Tags[tag] = desc.Digest.String()
		}
	}

	// Checked separately rather than inferred from the tags: a build may have no tags, and a tag
	// resolving does not prove the digest reference survived a storage wipe.
	if prev != nil && prev.Digest != "" {
		ref, err := name.ParseReference(fmt.Sprintf("%s@%s", repo, prev.Digest), refOpts...)
		if err == nil {
			if _, err := remote.Head(ref, opts...); err == nil {
				state.Digest = prev.Digest
			}
		}
	}
	return state, nil
}

// IsNotFound distinguishes "the registry says this is gone" from "the registry did not answer".
//
// A 404 means something is ALREADY lost; a timeout only that it might be. Anything acting on a loss
// must treat only the first as one, or a registry outage becomes a cluster-wide stampede.
func IsNotFound(err error) bool {
	var terr *transport.Error
	if errors.As(err, &terr) {
		return terr.StatusCode == http.StatusNotFound
	}
	return false
}
