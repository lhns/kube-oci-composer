# 27. What rootless BuildKit actually needs

Date: 2026-08-18

## Status

Accepted. Amends [0025](0025-dockerfile-builds-as-a-second-kind.md), which listed this as one of
the things that would settle whether `DockerBuild` survives.

## Context

0025 said the alpha is the experiment, and named an abandon criterion: *"Rootless BuildKit does not
run on the target nodes. Privileged is not an acceptable fallback — 0001:56-57 named that blast
radius as the original reason for refusing."*

The first end-to-end run against a real cluster answered it, and the answer was no:

```
[rootlesskit:parent] error: failed to setup UID/GID map:
  newuidmap 29 [0 1000 1 1 100000 65536] failed:
  fork/exec /usr/bin/newuidmap: operation not permitted
could not connect to unix:///run/user/1000/buildkit/buildkitd.sock after 10 trials
```

The build pod ran as uid 1000 with `allowPrivilegeEscalation: false`, `drop: ALL`, and seccomp and
AppArmor unconfined. buildkitd never started, so every build failed within seconds.

### Why rootless needs a privilege at all

"Rootless" means no root **on the host**. It does not mean no privilege is needed to start.

Building an image means creating files owned by many UIDs: `/etc/passwd` owned by uid 0, a package's
files owned by some service account. A process running as uid 1000 with no privilege cannot create
a file owned by uid 0. Linux answers this with user namespaces: inside a new namespace uid 1000 is
mapped to appear as 0, and a range of host UIDs appears as 1–65536 inside. Root inside is
unprivileged outside. That is what makes it rootless.

The catch is that the kernel lets an unprivileged process create a user namespace with exactly
**one** mapping. Mapping a **range** requires writing several entries to `/proc/<pid>/uid_map`, and
the kernel requires `CAP_SETUID` for that. Distributions ship `newuidmap`, a setuid-root helper
whose only job is that one write, validated against `/etc/subuid`. The requested range is visible in
the error above: uid 0 inside = 1000 outside, then 65536 more starting at 100000.

So the privilege is a one-shot bootstrap, not standing power. Once buildkitd is running it holds
nothing on the host.

The posture refused it twice over. `allowPrivilegeEscalation: false` sets `NO_NEW_PRIVS`, which makes
the kernel ignore the setuid bit entirely; and `drop: ALL` empties the bounding set, so `CAP_SETUID`
could not have been acquired even without `NO_NEW_PRIVS`.

## Decision

Build containers run with `allowPrivilegeEscalation: true`, all capabilities dropped, and exactly
`SETUID` and `SETGID` added. Everything else is unchanged: uid 1000, `runAsNonRoot: true`,
`privileged: false`, seccomp and AppArmor unconfined, no host namespaces, no device access, no host
mounts. The controller's own posture is untouched — distroless, non-root, read-only root filesystem.

Six configurations were measured on the e2e cluster (Kubernetes 1.36.1, containerd 2.3.1, kernel
6.17) rather than argued about. Two worked:

| configuration | result |
|---|---|
| escalation permitted, `drop: ALL` + `add: [SETUID, SETGID]` | **works** |
| escalation permitted, no capabilities stanza (upstream's own example) | **works** |
| escalation permitted, `drop: ALL`, nothing added | fails — bounding set is empty |
| `hostUsers: false`, four variants (rootless and standard images) | fails — pod never starts |

The first is what we take. Upstream's Kubernetes example ships the second, which leaves the
runtime's default capability set — roughly fourteen, including `CHOWN`, `DAC_OVERRIDE` and `FOWNER`.
Since the narrower one demonstrably works, there is no reason to carry the wider one.

## Consequences

**A setuid binary inside a build image can acquire `SETUID`/`SETGID` within the container.** That is
the real cost and it should not be softened. It is not host root: no `SYS_ADMIN`, no devices, no host
filesystem, and the process is uid 1000 throughout. The blast radius 0001 refused — a container with
host-level power — is not reinstated, and `privileged` is still not offered at any setting. But this
is a container running code from somebody's git repository, and it can now run setuid binaries from
its own image. Anyone weighing whether to install the builder should weigh that, and it is a further
reason the builder is a separate component with its own chart and RBAC rather than a flag on the
composer ([0004](0004-dockerfile-support.md)).

**0025's abandon criterion is not met, on a reading worth stating.** Rootless BuildKit does run;
`privileged` was never required. The criterion said privileged is not an acceptable fallback, and it
has not been used. A stricter reading — that *any* loosening triggers abandonment — is available, and
was considered. It is rejected because it would abandon over two capabilities scoped to a UID
mapping while the thing actually named, host-level privilege, is untouched.

**Kubernetes user namespaces would have cost nothing, and do not work here.** With
`hostUsers: false` the kubelet creates the namespace and installs the mapping before the container
starts, so no setuid helper is involved and `allowPrivilegeEscalation: false` with `drop: ALL` could
have been kept — with *stronger* isolation than today, since the container's root maps to an
unprivileged host uid. All four variants hung in `ContainerCreating` and never started. The most
likely cause is that kind's nodes are themselves containers without the subuid/subgid ranges
containerd needs to allocate a pod user namespace; this was not confirmed, and the kubelet's stated
reason was not captured. It is the right destination and it is recorded here so the option is not
rediscovered from scratch: revisit when it can be exercised in CI rather than assumed, because a
security posture that only exists where it cannot be tested is not one this project should ship.

**The e2e now covers the thing that was only ever assumed.** The security context is asserted in
`internal/buildcontroller/job_test.go` — exactly `drop: ALL` plus those two capabilities, and no
others — so widening it is a test failure rather than a review oversight. The previous assertion
encoded a configuration that provably cannot run, which is the more useful lesson: it passed on
every unit run while being wrong about the only environment that mattered.

**0025's first criterion is still open.** Whether two runs of the same context produce the same
output digest has not been measured; the builds had not yet run when this was written.
