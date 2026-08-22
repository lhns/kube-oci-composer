# 35. A registry is the only publication path

Date: 2026-08-21

## Status

Accepted. **Supersedes [0032](0032-the-embedded-registrys-future.md)**, accepted one day earlier,
and [0006](0006-push-is-optional.md), which made `push` optional by giving the controller its own
serving endpoint.

Also settles four records that only ever described that endpoint:
[0012](0012-keep-pkg-registry.md) (keep `pkg/registry`), [0013](0013-persist-manifests.md)
(persist manifests), [0014](0014-pluggable-storage.md) (a pluggable blob store) and
[0021](0021-active-standby-or-shared-storage.md) (active/standby vs shared storage). None is
edited: each described a real decision about a component that no longer exists.

## Context

0032 kept the embedded endpoint and named the condition under which that should be revisited:

> if the bundled registry makes single-component installation cheap enough that the distinction
> stops mattering, a new record supersedes this one.

[0033](0033-one-chart-one-namespace.md) is that condition arriving. The chart
now ships zot enabled, generates its own credentials and wires both controllers to it, so the
promise 0006 was protecting — install one thing, publish nothing, manage no credentials — is
delivered by `helm install` with no arguments. The embedded endpoint was the mechanism for a
property that now has a better mechanism.

What kept it alive until now was that removing it would have made a registry a hard prerequisite.
It no longer does; the prerequisite ships in the box.

**Two publication surfaces was the actual cost.** Not the 3,000 lines — the fork in every path
downstream of them. `spec.publish` beside `spec.push`, with a CEL rule to forbid both. `target()`
branching on which one was set. A readiness gate that meant "the store is warm" on one path and
nothing on the other. A refresher that had to ask whether an object was even a registry object. A
garbage collector whose sweep existed only for a store nothing else read. Each was small; together
they were the reason most questions about this project had two answers.

0032's own defect list is unchanged and still the supporting evidence: per-replica tag divergence,
a Ready-but-empty replica, `416` on resumed pulls, and an unauthenticated write path that
contradicted two documents asserting it was loopback-only.

## Decision

**The controller uploads to a registry. That is the only way an artifact becomes pullable.**

- `internal/serve`, the served blob/manifest store, replay, active/standby and `internal/gc` are
  deleted. Retention is [0031](0031-the-retention-guarantee.md)'s refresher against a real registry.
- **`spec.publish` is removed. `spec.push` is the only publication block**, and
  `push.repository` becomes optional: omitted means the default registry
  ([0034](0034-a-default-registry.md)), at `<registry>/<namespace>/<name>`.
- On `ImageBuild`, `push` stops being required for the same reason.
- Flags removed: `--serving-host`, `--serving-bind-address`, `--shared-storage`,
  `--standby-replay-interval`, `--gc-interval`, `--gc-grace`, `--gc-dry-run`. The layer **cache**
  (`--cache-dir`, S3) is untouched — that is input caching and has nothing to do with serving.

**`push` and `publish` were the same thing wearing two names.** They differed only in where the
bytes landed, and with one destination left there is nothing for two fields to distinguish. Users
asking what the difference was is how this was noticed.

## Consequences

**This is a breaking API change, and a silent one if handled carelessly.** An object with
`publish:` and no `push:` is not rejected by an older CRD's validation — it simply publishes
nowhere. The CHANGELOG carries the field-by-field migration, and the upgrade order matters: the
chart's CRDs now upgrade with the release ([0033](0033-one-chart-one-namespace.md)),
so `helm upgrade` applies the new schema, and objects still carrying `publish` fail validation
loudly at that point rather than going quiet. That is the intended failure.

**The strongest thing 0006 promised is genuinely gone: no `hosts.toml`.** The embedded endpoint was
an ordinary Service behind the cluster's ingress and certificate, so containerd needed no node
configuration at all. A bundled registry reached over a NodePort does need a drop-in, because
containerd resolves image references with the node's resolver and cluster DNS is not it. An
operator who fronts the registry with an ingress and a real certificate gets 0006's property back;
one who does not pays a per-node file. `docs/registry.md` says so plainly, and the chart's NOTES
warn when `registry.host` is unset, because the failure otherwise is a successful publish followed
by `ErrImagePull`.

**The single-node install 0006 and 0032 were protecting still works, and now stores its images
durably.** It costs one more pod and a PVC. What it gains is a store built to be a system of record
— which matters most for `ImageBuild`, whose output cannot be rebuilt from its spec
([0025](0025-dockerfile-builds-as-a-second-kind.md)) and which 0032 had already stopped letting the
embedded endpoint serve.

**Determinism stops being load-bearing for availability.** 0006 leaned on it: a lost blob store cost
a rebuild, not data. That is still true of compositions and still worth having, but it is no longer
what keeps artifacts pullable. The registry's storage is, which is why the chart's NOTES warn when
persistence is off.

**Spegel's role is unchanged** and is the reason registry downtime is not a cluster-wide outage:
once an image is on a node, Spegel serves it peer-to-peer. It was never a push target; that has not
changed.

**The reversal is one day old, and that is worth naming.** 0032 decided this correctly on the
information it had — the bundled registry was off by default and unproven when it was written.
What changed is not the reasoning but the world it reasoned about: 24 hours later the registry
ships on, generates its own credentials, and has a measured retention guarantee behind it. A record
that cannot be superseded that fast is a record that outranks evidence.
