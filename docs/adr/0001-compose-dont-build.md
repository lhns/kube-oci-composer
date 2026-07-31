# 0001. Compose, don't build

## Status

Accepted. This is the constraint everything else follows from; nothing later in this series
makes sense without it.

## Context

Workloads routinely need files their upstream image does not ship: plugin JARs for a broker,
extensions for a feed reader, custom components for a home automation platform. Every workaround
is bad in a different way — a static PV mounted over the path, so the version lives in a
filesystem rather than in git; an initContainer that clones a repository at runtime; a stateful
in-application installer. The one workload that has this solved bundles the files into its image,
which is to say the problem is *getting extra files into a container you do not build*.

Kubernetes now answers the consumption half: `spec.volumes[].image` mounts an OCI artifact. What
is missing is something to produce the artifact.

Flux deliberately does not build images. `image-reflector-controller` scans registries and
`image-automation-controller` writes digests into git; both *consume*. Building was excluded
because it is non-deterministic and side-effecting, which is incompatible with a controller that
must converge.

That exclusion is correct — and it does not apply if we **compose** rather than **build**.

## Decision

`ImageComposition` assembles an OCI artifact from content-addressed inputs. No Dockerfile, no
`RUN`, no arbitrary execution. Layers are unpacked and placed at declared paths, and the result is
written out.

The output digest is therefore a pure function of the spec. That single property is what makes
everything else possible:

- **Idempotent** — the same spec always produces the same digest, so republishing is a no-op.
- **Convergent** — the controller can compute what *should* exist and compare it to what does,
  which is the entire reconcile loop.
- **Cheap to reconcile** — comparing a hash beats rebuilding, so an hourly interval costs one
  `HEAD` (see 0002).
- **Honestly attestable** — provenance is exact rather than scanned, because every input is
  already named by digest (see 0008).

## Consequences

It cannot compile anything. Anything needing a compiler is served by ordinary CI, and if
in-cluster building is ever wanted it belongs to a separate kind with a weaker promise (0004).

For the cases that motivated this — plugin JARs, extensions, components — composition is the
whole answer, not a subset of it.

## Alternatives rejected

**Run a Dockerfile.** The output digest cannot be known without building, so the reconcile loop
would have to rebuild to discover whether a rebuild was needed. It also needs a privileged or
rootless-BuildKit pod, raising the blast radius from "a controller that reads URLs" to "a
controller that executes arbitrary code from a git repository".

**Wait for Flux to add it.** It was excluded on purpose and the reasoning still holds for
building. Composition is a different thing and does not require the exclusion to be revisited.

**Bundle the files into a custom image in CI.** Works, and is what the one solved workload does.
It moves the version out of the cluster's git repository into a separate pipeline, and it means
one image build per upstream release — coupling the artifact's cadence to the base image's.
