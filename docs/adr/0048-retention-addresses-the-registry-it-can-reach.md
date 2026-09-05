# 48. Retention addresses the registry it can reach

## Status

Accepted.

## Context

A 0.5.0 cluster reported the same error every hour for three days, on nine objects:

```
level=error logger=retention msg="refresh failed"
  object=jetkvm/jetkvm-cloud-api consecutiveFailures=72
  error="5 of 9 references: Get https://oci-composer.internal:30500/v2/:
         dial tcp: lookup oci-composer.internal on 172.19.0.10:53: no such host"
```

**Five of nine.** That split is the whole diagnosis. `refsOf` builds digest references from the
repository it resolved and takes tag references from status:

```go
add(digestRef(repo, artifact.Digest))     // repo + "@" + digest -- in-cluster, correct
for _, tag := range artifact.Tags {
    add(qualify(repo, tag))               // returned the stored value verbatim
}
```

And both controllers write `status.artifact.tags` through `PublicRepository` — the Ingress or
NodePort name a *workload* pulls from, which a pod deliberately cannot resolve. So digests refreshed
and tags did not.

**It had already destroyed content.** `github-runner/github-runner` has no tags left; its manifest
survives only as an untagged blob, which is exactly what the shipped `deleteUntagged` policy
reclaims next. Its `ImageBuild` reports `Ready=True`, because publishing succeeded — a year ago is
as good as a minute ago to a reconciler that only checks its own inputs.

This defeats [ADR 0031](0031-the-retention-guarantee.md), which is the guarantee this project is
least willing to lose.

### The invariant was written down three times

- `defaultregistry.go` — *"Everything that actually talks to a registry … **the retention refresh**
  — uses `RepositoryFor`"*
- `imagebuild_controller.go` — *"the retention refresh … goes through `repositoryFor`"*
- `charts/…/_helpers.tpl` — *"Rendered into `status.artifact.ref` and nowhere else."*

All three were false for as long as they existed. The most confident of them sat on
`PublicRepositoryFor`, a function with **no callers outside its own test**.

Comments do not hold invariants. Neither does stating one three times.

### Why the tests could not catch it

Every fixture in `refresher_test.go` wrote tags as `repo + ":v1"`, using the same `repo` the
refresher resolves. The two hosts could not disagree, so the bug had nowhere to appear. The fixture
encoded the assumption production violates, which is the most durable way to make a test suite
agree with a defect.

## Decision

**Retention rebuilds every reference from the repository it resolved, and trusts nothing stored.**
`qualify` reduces a stored value to its bare tag and re-qualifies it, which makes it symmetric with
`digestRef` — already correct, and correct for the same reason.

Parsing is the fiddly part and is stated once, in `bareTag`: the tag is what follows the last `:`
**after** the last `/`, because a host carries a colon of its own; a digest reference is refused
before that rule runs, since a digest also contains one; a value naming a repository with no tag
yields nothing rather than being appended to `repo` as though it were a tag.

**Rebuilding history tags against the current repository is not a new assumption.**
`digestRef(repo, rec.Digest)` has always done exactly that for history digests.

**`PublicRepositoryFor` is deleted.** No caller, and it carried the invariant's most confident
statement. What survives moves onto `PublicRepository`, which the controllers actually call.

### The mechanism, which is not the comment

Three things, none of them prose:

1. **Every fixture uses two different hosts.** Status tags are written through a public host that
   resolves nowhere while the refresher resolves the test registry. On the shipped code, two
   existing tests fail — verified by reverting `qualify` alone.
2. **An invariant assertion**: every reference `refsOf` returns must begin with the resolved
   repository. One loop, and the class of bug cannot return by any route.
3. **A truth table for `bareTag`**, which caught a hole while being written: `app@sha256:…` parsed
   as the tag `…` until digests were refused first.

## Consequences

**A tag published to a repository the object no longer names is no longer refreshed.** Retention
follows the object's *current* repository. This is not new — history digests already behaved this
way — but it is now true of tags too, and it is the right reading: the guarantee is about what a
live object references, and an object that moved has stopped referencing the old location.

**Recovery is not automatic for `ImageBuild`.** An `ImageComposition` re-publishes identical bytes
on its next reconcile, because `ResolvePublished` notices the missing tag and `output = f(spec)`.
An `ImageBuild` cannot reproduce a digest, so what it should do about a missing artifact is a
separate decision, taken separately.

**Anything already reclaimed stays reclaimed.** This stops the loss; it does not undo it.

**The comments that were false now describe what the code does.** That is worth almost nothing on
its own, and is recorded here so the next reader knows the fixtures are the load-bearing part.
