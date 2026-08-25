# Threat model

STRIDE over the two components this project ships. Written against the code, not against an
intended design: every claim below names the file that makes it true, so it can be checked and so it
rots visibly when the code moves.

Scope is what this repository controls. The registry, the Flux source controllers, the container
runtime and the cluster's own admission policy are dependencies with their own models; where a
guarantee actually rests on one of them, that is stated rather than assumed.

## Components and trust boundaries

The two components are deliberately separate: separate binaries, charts and RBAC
([ADR 0004](adr/0004-two-kinds-two-controllers.md)). Installing the composer does not install the builder,
and the composer's role cannot create a single object.

```mermaid
graph TB
    subgraph tenant["Tenant namespace (untrusted input)"]
        IC["ImageComposition"]
        DB["ImageBuild"]
        CM["ConfigMap"]
        SEC["Secret<br/>push + build credentials"]
        SRC["Flux source<br/>GitRepository / OCIRepository / Bucket"]
    end

    subgraph releaseNS["release namespace (one chart, ADR 0033)"]
        COMP["kube-oci-composer<br/>distroless, non-root, read-only rootfs"]
        BUILD["kube-oci-builder<br/>distroless, non-root"]
        ZOT["Bundled registry (zot)<br/>anonymous read, authenticated write"]
        STORE[("Registry storage<br/>PVC, or S3 when clustered")]
        CACHE[("Layer cache<br/>disk or S3 — inputs only")]
        KEYS["Operator secrets<br/>registry password · TLS key · cosign key"]
    end

    subgraph node["Node (shared kernel)"]
        JOB["Build Job pod<br/>rootless BuildKit<br/>runs code from a git repo"]
        COPIED["Copied into the tenant namespace<br/>push credential + registry CA<br/>owned by the ImageBuild"]
    end

    EXT["External origins<br/>HTTP URLs, upstream registries"]
    REG["Target registry<br/>the bundled zot by default, or one you supply<br/>anonymous read, authenticated write<br/>expires what is not pulled"]
    CONSUMER["Workloads pulling images"]

    IC -->|watch| COMP
    CM -->|watch| COMP
    SRC -->|get artifact| COMP
    SEC -->|get| COMP
    KEYS -->|get| COMP
    KEYS -->|get| BUILD
    COMP -->|fetch by digest| EXT
    COMP --> CACHE
    ZOT --> STORE
    ZOT -.->|"the default target, in-cluster"| REG
    COMP -->|push| REG
    COMP -->|"refresh: pull only,<br/>renews the retention lease"| REG
    REG -->|pull| CONSUMER

    DB -->|watch| BUILD
    SRC -->|get artifact| BUILD
    SEC -->|get| BUILD
    BUILD -->|"refresh: pull only"| REG
    BUILD -->|create Job| JOB
    BUILD -->|"create Secret<br/>(cross-namespace, E6)"| COPIED
    COPIED -->|mount| JOB
    JOB -->|fetch context| EXT
    JOB -->|push| REG
    JOB -.->|digest via termination message| BUILD

    classDef trusted fill:#1b5e20,stroke:#a5d6a7,color:#ffffff
    classDef hostile fill:#b71c1c,stroke:#ef9a9a,color:#ffffff
    classDef neutral fill:#0d47a1,stroke:#90caf9,color:#ffffff
    class COMP,BUILD trusted
    class JOB,EXT hostile
    class IC,DB,CM,SEC,SRC,STORE,CACHE,ZOT,REG,CONSUMER,KEYS,COPIED neutral
```

The boundaries that matter:

| Boundary | Crossing | Why it matters |
|---|---|---|
| **Tenant → controller** | A spec is written by anyone with `create` on the CRD | Spec fields become URLs fetched, images pulled, and Jobs run |
| **Origin → controller** | Fetched tarballs, pulled base images | Attacker-controlled bytes parsed in-process |
| **Controller → build pod** | A Job running BuildKit | The pod executes arbitrary code from a git repository |
| **Registry → consumer** | A pull, by any client that can reach the registry | Anonymous by default, so reachability is the access control (I5) |
| **Namespace → namespace** | `sourceRef.namespace` | The one place a tenant reaches outside its own namespace |
| **Release ns → tenant ns** | The builder copies a push credential and the registry CA next to each build | A cluster-wide `create` on Secrets, and the only thing this system writes outside its own namespace (E6) |

## The core invariant, and what it buys

`output digest = f(spec)` for `ImageComposition`. Assembly pins mtimes to epoch, forces empty
`uname`/`gname`, normalises modes, sorts entries with `sort.SliceStable`, and discards source-side
gzip metadata (`internal/oci/assemble.go`, `internal/oci/extract.go`).

Security consequence: **content cannot change without the spec changing**, so review of the spec is
review of the artifact. `publish.onConflict: Fail` (the default) refuses to move a tag that already
resolves to different bytes — which is what caught a real incident where a tag was published holding
a previous revision's content ([ADR 0026](adr/0026-a-source-artifact-can-lag-its-own-spec.md)).

For `ImageBuild` the invariant is weaker by construction: the output is an *observation*, not a
function of the spec ([ADR 0025](adr/0025-dockerfile-builds-as-a-second-kind.md)). Rebuilds do
reproduce for builds whose steps are themselves deterministic ([ADR 0027](adr/0027-what-rootless-buildkit-actually-needs.md)),
but a `RUN` that installs packages or reads the clock can still differ. The tag-conflict guard is
therefore load-bearing rather than decorative for this kind — and it did not exist on this kind at
all until [ADR 0029](adr/0029-three-valued-tag-conflict-policy.md): the field was in the CRD and
nothing read it, so every build overwrote whatever its tags held.

## S — Spoofing

| # | Threat | Status | Evidence |
|---|---|---|---|
| S1 | A client impersonates the controller and **writes** published images | **Mitigated by the registry** | The serve endpoint this row was written about no longer exists (ADR 0035). The bundled zot enforces `anonymousPolicy: [read]` and requires the generated credential for `create`/`update`, so an arbitrary pod cannot write. That is a stronger guarantee than the one it replaces, and it is enforced by a component with its own test suite rather than by a handler in this repo. **The history is worth keeping**: this row once read "partially mitigated — depends on deployment", which was wrong, and then "mitigated" via a loopback guard added only after a test confirmed an arbitrary pod's `PUT` returned **201 Created**. ADR 0025:87-90 rested on the same false premise. |
| S2 | A registry impersonates the origin of a base image or layer | **Mitigated** | Base images and image layers are pulled by digest only — `name.NewDigest` fails on a tag (`internal/source/image.go`), and the CRD pattern requires `@sha256:`. Fetched layers are verified against the declared digest (`internal/oci/fetch.go`). |
| S3 | A build pushes to a registry impersonating the intended one over plain HTTP | **Mitigated, opt-in per host** | `--insecure-registry` is a list of hosts, matched on the push target's host rather than applied globally (`insecureAttr`, `internal/buildcontroller/job.go`). Naming one internal registry does not downgrade every other push. |
| S4 | Someone signs an image as this operator | **Bounded by Secret access, and not otherwise** | The cosign key lives in one Secret in the release namespace (`--signing-key-secret`). Anyone who can read it can sign anything, and every signature this operator ever made stays valid: there is no revocation, no transparency log for an extra signature to show up in (key-based, not keyless, by ADR 0008), and no rotation story. The controllers grant themselves `get` on secrets and never `list`/`watch`, so the exposure is whoever else can read Secrets in that namespace, which is already the set that can read the registry password. Treat the two as one blast radius. |
| S5 | A signature is read as approval | **Not mitigated, because that is not what it means here** | Any tenant who can create an `ImageComposition` in any namespace gets the operator's signature on whatever they composed, because the operator is what published it. The signature attests **provenance, not review**: "this came out of our pipeline", not "somebody looked at it". A policy requiring it asserts the former only. It is not a substitute for RBAC on the CRD, and `docs/examples/verify` says so. |
| S6 | A registry impersonates the bundled one to a controller | **Mitigated when TLS is on; open by default** | With `registry.tls.enabled` the controllers verify the certificate against a CA pinned by `--registry-ca-file`, additive to the system roots. Without it they speak plain HTTP to a Service name and authenticate anyway, so anything that can win that name collects the push password (I7). The TLS private key is a new asset of the same weight as the password: whoever holds it can be the registry. |

**Reads remain anonymous by design** — a kubelet pulls without credentials — but that is now a zot
policy an operator can change, rather than a property of code here that could not be changed at all.
In a multi-tenant cluster a NetworkPolicy is still what separates one namespace's artifacts from
another's readers.

The lesson worth keeping from S1: the guarantee had been *written down* in a package comment for a
long time without ever being *implemented*, and nothing tested it. A claim about behaviour is not
behaviour.

## T — Tampering

| # | Threat | Status | Evidence |
|---|---|---|---|
| T1 | Layer content changes without the spec changing | **Mitigated; optional pinning, cluster-enforceable** | `fetch` carries a declared digest. For `sourceRef`: an artifact that predates its own source's spec is refused (`internal/source/flux.go`, ADR 0026), and `sourceRef.revision` pins the revision a layer expects. The pin is what covers a **branch or semver range**, which moves with no generation bump for the staleness check to see. Pinning stays optional per ADR 0026 — tracking a branch is a legitimate thing to want — but `--require-pinned-sources` now lets an operator refuse unpinned sources for a whole cluster, on both kinds. Objects that omit `revision:` go Stalled naming the flag. |
| T2 | A published tag is repointed at different content | **Mitigated by default** | `onConflict: Fail` (the default on both kinds) refuses to move a tag resolving to a different digest, and on `ImageBuild` the check runs *before* the Job, since a push from inside it cannot be undone. Two inherent limits: it cannot validate a tag's **first** publish, because there is nothing to compare against; and `onConflict: Overwrite` disables it by design. |
| T3 | A malicious archive escapes the target directory on unpack | **Mitigated** | Traversal is refused rather than sanitised (`internal/oci/extract.go`): any entry whose cleaned path is `..` or starts with `../` is an error. Zip entries normalise `\` to `/` **before** the check (`internal/oci/zip.go`). |
| T4 | A build tampers with another build's cache | **Mitigated** | The cache ref is per-object, derived from namespace and name (`cacheRefFor`). A shared cache would be a channel between whoever can write Dockerfiles. |
| T5 | The controller is upgraded to assemble differently, silently | **Mitigated by a golden-digest test** | `TestAssembleMatchesItsGoldenDigest` (`internal/oci/assemble_test.go`) pins the assembled bytes: change the tar writer, the gzip level, the config, the ordering or the toolchain's flate output and it fails, naming `AssemblyVersion` in the failure. It also refuses to run if `AssemblyVersion` has moved without the digest being re-recorded, so the two cannot drift apart. What stays manual is the deliberate bump once the test has told you the output changed — but the *silent* case this row is about is caught by mechanism, not discipline. |
| T6 | A build pod tampers with the node or other pods | **See E1** | |
| T7 | A second registry process serves or corrupts content | **Mitigated by construction; the earlier clustering answer was withdrawn** | This row previously covered a *sharded cluster member* being impersonated, and the mitigation was inter-member TLS. Members no longer exist: read replicas never talk to each other, so there is no proxied hop to intercept and the TLS requirement that guarded it was removed with its reason (ADR 0041). What replaced the threat is D12 -- the danger is not a rogue peer but any second process that WRITES the shared store, including one that merely garbage-collects. |
| T8 | Changing the shard key redistributes content | **Not a tampering path; recorded to head off the assumption** | `cluster.hashKey` decides which member owns which repository. Changing it reshuffles everything, but with shared S3 storage nothing is lost -- the bytes stay reachable. It is generated once and kept in a Secret so it cannot move by accident, and a supplied key of the wrong length is refused: siphash-2-4 needs 128 bits, and zot's failure on a short key is not legible. |
| T9 | A tenant supplies the build recipe without write access to any source | **Accepted, and it is a smaller shift than it looks** | `spec.dockerfile.inline` means creating an `ImageBuild` is enough to decide what gets built, where before the recipe had to be pushed to a repository the object referenced. The shift is narrow: whoever could create the object already chose which context it read, and `imageBuild.enabled` is documented as granting the ability to run arbitrary containers in that namespace (ADR 0025) -- an inline Dockerfile is a more direct route to a capability the object already conferred, not a new one. What does NOT change is the unpinned-`FROM` guard: it runs on the resolved bytes whatever their source, so an inline recipe cannot reach an unpinned base, and a test asserts that per source. |

## R — Repudiation

| # | Threat | Status | Evidence |
|---|---|---|---|
| R1 | An artifact exists and nobody can say what produced it | **Mitigated** | Three records now, in descending order of durability. `status.history[].sources` carries each layer's name, resolved digest and revision -- what ADR 0026's incident needed and did not have. OCI **manifest annotations** put the same summary in the artifact, so it survives the object. And with `supplyChain` enabled, a full **SPDX SBOM and SLSA provenance** are attached as OCI referrers (ADR 0040), signed when a key is configured. For an `ImageComposition` the SBOM is derived from digest-pinned inputs and is exact; for an `ImageBuild` it comes from BuildKit and is a scan of the result, because only the build can see what it installed. All of it is off by default. |
| R2 | A failure leaves no trace after the pod is gone | **Mitigated** | A failed build's Job is kept for the whole backoff so its pod's logs survive; the exit code, reason and termination message are copied into status and raised as an Event. |
| R3 | An attestation is believed without being signed | **By design; the two switches are independent** | With no signing key configured, the SBOM and provenance attach as **bare in-toto statements**. That is the honest shape for "here are the facts, unsigned" -- anyone who could write to the repository could have written them, so they say nothing about origin. With a key, the same statements are wrapped in a signed DSSE envelope. Enabling `supplyChain.sbom` without `supplyChain.signing` produces documentation, not evidence, and reading it as evidence is the mistake this row exists to name. |

R1 is materially better than it was: `status.history[].sources` now records each layer's name,
resolved digest and revision, so *"which source revision produced this digest?"* is answerable from
the API. What is still missing is provenance **in the artifact** — no OCI annotations carry it — so
the answer exists only while the object does.

## I — Information disclosure

| # | Threat | Status | Evidence |
|---|---|---|---|
| I1 | Secret **values** leak through `status.inputHash` | **Mitigated** | Only `name` + `resourceVersion` reach the hash (`internal/buildcontroller/imagebuild_controller.go`). Status is readable by anyone with `get`, and a hash of a low-entropy secret is an oracle. |
| I2 | Secret values leak through build args | **Mitigated** | Secrets are projected via BuildKit's secret mount, never as `--opt build-arg`. Build args *are* hashed, so anything placed there is world-readable by design. |
| I3 | The controller holds credentials it does not need | **Mitigated** | Both roles grant `get` on secrets — never `list` or `watch` — and a chart drift guard fails the build if that ever changes (`TestBuilderChartNeverGrantsSecretListOrWatch`). Push credentials for builds are projected straight into the build pod and never read by the controller. |
| I4 | A tenant reads another namespace's source content | **Mitigated** | A `sourceRef` naming any namespace but the object's own is refused with a terminal error (`internal/controller/resolve.go`), and `ImageBuild.spec.context.sourceRef` the same. Narrowed from `spec.context` when that field became a union (ADR 0042): the other members carry no namespace at all, so the boundary is the one reference that can name one. It had to be fixed controller-side rather than in CEL: a CRD validation rule cannot read `metadata.namespace`. Both controllers still hold cluster-wide `get;list;watch` on Flux sources, so this rule is the whole of the boundary — which is why it is asserted by tests on both kinds. Secrets and ConfigMaps were never exposed this way; both always resolved against `obj.Namespace`. |
| I5 | Anyone on the network pulls any published image | **Deliberate default, now configurable** | The bundled registry allows anonymous reads, because a kubelet pulls without credentials. Any client that can reach the registry pulls every artifact in it, across all namespaces. What changed with ADR 0035 is that this is zot's `anonymousPolicy`, so an operator who wants authenticated pulls can have them and put imagePullSecrets on the workloads — where before, `internal/serve` had no authentication to enable. |
| I6 | An SSRF via a `fetch` URL reaches cluster-internal services | **The credential case is closed; the rest is opt-in** | Link-local (`169.254.0.0/16`, `fe80::/10`) is refused unconditionally, which covers the cloud metadata endpoint every major provider serves credentials from (`internal/oci/dialguard.go`, ADR 0036). Other private ranges — RFC1918, loopback, unique-local, CGNAT — are refused only under `--fetch-deny-private`, because an artifact server on a private address is this project's most ordinary layer source and a guard that refuses those gets disabled. Enforced in `net.Dialer.Control`, after resolution and before `connect(2)`, so a hostname pointing at the metadata IP, a redirect to it, and a DNS rebind are all caught — none is visible in the URL. **With the flag off, which is the default, a tenant can still make the controller `GET` an internal address.** This now covers the BUILDER too: `ImageBuild.spec.context.fetch` made it read a user-supplied URL for the first time, where before the only URL it fetched came from source-controller's own status, so the same guard and the same `--fetch-deny-private` were added there. The fetch inside the BUILD POD is deliberately unguarded and should stay so -- that pod is about to run arbitrary code from a Dockerfile and can already reach whatever the pod network allows, so a guard there would block in-cluster registries while preventing nothing. Note `network: None` restricts BuildKit's `RUN`, not the init container. |
| I7 | The registry write credential is observable on the wire | **Closeable, opt-in; open by default** | zot can now terminate TLS itself (`registry.tls`, ADR 0038), with the certificate from cert-manager, a Secret you supply, or a CA the chart generates — and the CA is distributed to both controllers and, through the same cross-namespace mechanism as the push credential, to build Jobs. With it on, the credential no longer crosses the pod network readable. **Off by default**, because zot serves one scheme at a time and turning it on invalidates the containerd drop-in on every node; a release that flipped it would produce images nothing could pull. So this row stays honest: on a default install the password still travels in an HTTP Basic header, and closing it is one value plus a per-node change. |
| I8 | An SBOM tells an attacker exactly what to attack | **Deliberate, and it follows from I5** | Attestations live in the registry, whose reads are anonymous by default. An SBOM -- a list of precisely which versions are inside your images -- is therefore readable by anything that can reach it, which is both the point of publishing one and a reconnaissance aid. `provenance: max` on an `ImageBuild` goes further and publishes the **Dockerfile and build arguments**; the default is `min`. If reads are not restricted, do not turn `max` on. |
| I9 | The registry's object-storage and cache credentials leak | **Mitigated** | S3 credentials reach zot as environment variables from a Secret (`registry.storage.s3.existingSecret`), never in `config.json` -- a ConfigMap is readable in every `kubectl describe`, the same rule `TestChartCredentialsAreNotFlags` enforces for the composer's own S3. Redis is the awkward case, because zot's driver accepts credentials only inside the URL (`redis://user:pass@host`): when that URL carries credentials the **whole config is rendered into a Secret instead of a ConfigMap**. Worth recording that this row previously *claimed* that mitigation while the template rendered a ConfigMap unconditionally -- the claim was false until read replicas made redis mandatory and it was checked. `TestACredentialedCacheURLNeverLandsInAConfigMap` is what keeps it true. |
| I10 | The CA copied into a tenant namespace discloses something | **Not a disclosure; recorded because it looks like one** | The builder writes `<job>-registry-ca` into the object's namespace so the build pod can trust the registry. It holds a certificate **authority certificate, not a key** -- public by construction and useless to an attacker. It is a Secret only because the builder already holds `create`/`update` on Secrets, where a ConfigMap would need a new grant in every tenant namespace (E6). |

I5 is deliberate rather than overlooked, and it is the shape almost every in-cluster registry
ships in. Note what it means in a multi-tenant cluster: with the default policy, a NetworkPolicy is
the only thing separating one namespace's artifacts from another's readers.

## D — Denial of service

| # | Threat | Status | Evidence |
|---|---|---|---|
| D1 | A huge fetched artifact exhausts memory or disk | **Partially mitigated** | The Dockerfile pre-check is bounded (`maxDockerfileBytes`, `maxContextScan` in `internal/build/context.go`) and never writes to disk. Layer fetches stream to the cache, but there is no per-object size cap. |
| D2 | A build runs forever | **Mitigated** | `spec.timeout` becomes the Job's `ActiveDeadlineSeconds`, enforced by Kubernetes rather than by the controller noticing — so it survives a leader change. |
| D3 | A failing build hot-loops against the API server | **Mitigated** | Capped exponential backoff, and the failed Job is retained until its backoff elapses so that deleting it cannot wake the controller through its own watch. This was a real defect. |
| D4 | Builds exhaust node resources | **Partially mitigated** | `spec.resources` applies to both the build and fetch containers, but is optional; a namespace `ResourceQuota` is the real control and is the cluster's job. |
| D5 | Unbounded history growth in status | **Mitigated** | Rotation is capped by `historyLimit`, and a rebuild reproducing an earlier digest moves that entry rather than adding one. |
| D6 | A registry reclaims images live workloads are still running | **Mitigated, and it fails unsafe** | Both controllers re-pull every image a live object references (`--retention-refresh-interval`, default `1h`), which is what keeps a recency-based expiry policy from collecting them. A refresh only READS, so no bug in it can delete anything. The mitigation depends on the interval staying far below the registry's window — the RATIO is the guarantee — and on refreshing actually running: sustained failure raises `RetentionDegraded`, because the symptom of silence here is deletion one window later. See ADR 0031. |
| D7 | Refreshing is disabled or misconfigured against a registry that expires content | **Mitigated for the bundled registry; the operator owns it otherwise** | `--retention-refresh-interval=0`, or a window shorter than the interval, silently removes the protection in D6. For the registry the chart installs, both numbers are now rendered by one chart, so it **refuses to install** a margin below 24x or refreshing disabled while a window is set (`templates/_retention.tpl`, `TestChartRefusesARetentionMarginThatIsTooThin`). For a registry you supply, nothing here can read its policy, and the relationship is documented in `docs/registry.md` and unenforced. |
| D8 | A digest a workload still needs is reclaimed because no live object produces it any more | **NOT mitigated here, but narrower than it looks** | D6 refreshes what a live `ImageComposition` or `ImageBuild` references — its current artifact plus `status.history`, capped by `--keep-builds`. A digest older than that cap, or one whose object was deleted, is refreshed by nothing and expires. **Two layers absorb most of the impact.** The kubelet never garbage-collects an image a running container is using, so a running workload does not break when the registry forgets its image; and where Spegel is deployed, any node that still holds it serves it peer-to-peer to a node that does not. What is left is the case where **no node holds it any more and the registry has expired it**: a scale-to-zero followed by a scale-up, a node pool replaced or a cluster rebuilt, a rarely-run `CronJob`, or — the sharpest one — a **rollback** to a digest that has aged out of history, been expired by the registry, and been reclaimed locally once nothing was running it. See ADR 0019. |
| D9 | A non-persistent redis silently deletes live images | **NOT mitigated, and it cannot be from here** | `extensions.search` records the pull timestamps `keepTags.pulledWithin` depends on -- the mechanism the whole retention guarantee rests on (ADR 0031). Read replicas move that metadata out of per-pod BoltDB and into the shared cache driver, so **a redis restart without persistence loses every timestamp**: every image then looks unpulled, and the next GC pass reclaims images live objects still reference, on a `gcDelay` fuse rather than a retention window. The likelihood rose with ADR 0041: redis was previously reachable only through an opt-in mode with a discouraging paragraph attached, and is now the mandatory prerequisite of the availability feature people will actually enable. The chart cannot inspect redis, so this is stated in `values.yaml`, NOTES and `docs/registry.md` and owned by the operator. **It is the strongest single argument for `readReplicas: 0` remaining the default.** |
| D10 | A node drain takes the registry down with its own consumers | **Mitigated, with two named residuals** | The original failure: the registry had no scheduling controls at all, so a drain rescheduled it in the same batch as the pods pulling from it, which then sat in `ErrImagePull` waiting for it. Registry-scoped `nodeSelector`/`tolerations`/`affinity`/`priorityClassName` now place it away from the nodes being cordoned, and `readReplicas` puts more than one pod behind the read Service so a drain costs capacity rather than availability. The PodDisruptionBudget is what makes that true -- replicas alone do not survive a drain, because nothing stops an eviction taking all of them at once. **Residuals:** the default is still one pod, deliberately, since the shared store and redis are the operator's to run; and replicas are spread on a *preferred* rule, so the chart cannot guarantee they are on different nodes and a co-located set still dies with its node. |
| D11 | Attestations expire while the image they describe lives on | **Mitigated** | A referrer manifest is **untagged**, and the shipped policy is `deleteUntagged` with `keepUntagged.pulledWithin` -- so an SBOM or provenance statement would be reclaimed one window after it was written, silently, while its subject stayed alive. D6 on a new object type. The refresher now lists and pulls each refreshed digest's referrers, with a test that fails when the call site is removed. Signatures need nothing: a `.sig` is a tag, so `keepTags` already covers it. |
| D12 | A second process writes the registry's store | **Mitigated by construction, and it was measured** | zot serialises repository writes with an in-process mutex: a push is a read-modify-write of `<repo>/index.json`, and two processes doing it at once lose tags that returned `201` -- 2-4% per run in `test/spike`, with every instance then agreeing they were never written. **Collection is a write too**, so a replica that garbage-collects is a second writer; content being actively refreshed still went missing. So the writer StatefulSet is pinned to one pod, the Service the controllers push to selects it alone and is ClusterIP, and read replicas render `gc: false` with no retention policy. The failure is silent and in the deleting direction, which is why it is enforced in the chart rather than documented. Not a filesystem problem: object storage does not fix it, because the race is between processes. |
| D13 | The shared store is the new single point of failure | **Accepted, and named rather than sold** | Read replicas move the single point of failure from the registry pod to whatever backs the shared volume or bucket, which becomes the availability floor for every pull in the cluster. Three registry pods over one non-redundant fileserver is *worse* than one pod on a volume. Only worth enabling where that store is itself redundant, and NOTES says so at install. |
| D14 | A push is interrupted when the writer moves | **Mitigated by retry, and it is the cost of the design** | Only one pod accepts writes, so while it is being rescheduled every push fails. Tolerable because the reconcile is idempotent: a failed push is a delayed publish, not a lost one, and the next cycle re-pushes. Worth a row because it is the one place where the availability feature *causes* a failure that did not exist with a single pod -- anything needing synchronous publication should not rely on this. |

## E — Elevation of privilege

| # | Threat | Status | Evidence |
|---|---|---|---|
| E1 | **A build escapes the pod and reaches the node** | **Reduced, not eliminated — accepted** | See below. |
| E2 | A build uses the controller's API credentials | **Mitigated** | `automountServiceAccountToken: false` unless the spec names an identity. A pod running code from a git repository must not carry the token of whatever created it. |
| E3 | Installing the composer implies the ability to run containers | **Mitigated** | The builder is a separate component. ADR 0004 rejected a feature flag: *"a flag set to `false` is a weaker guarantee than a component that does not exist."* The composer's role cannot create a single object. |
| E4 | A tenant escalates by pointing a build at a privileged service account | **Partially mitigated** | `spec.serviceAccountName` is honoured, so a tenant who can create a `ImageBuild` can run a build under any service account **in their own namespace**. That is the same privilege they already have via a Pod, so it is not an escalation — but it is worth stating, because it means the builder inherits the namespace's Pod-creation trust model. |
| E5 | Someone reintroduces `privileged` | **Mitigated by test** | `privileged: false` is asserted for every container in the build pod, and the capability set is asserted **exactly** — `drop: ALL` plus `SETUID`/`SETGID` and nothing else. |
| E6 | The builder writes Secrets into any namespace | **Accepted; the narrowest grant that works** | The builder holds cluster-wide `create`/`update` on secrets -- never `list` or `watch` -- because a build Job runs in its object's namespace and a pod mounts only from its own, so the push credential and the registry CA must be copied there. Both copies are owned by the `ImageBuild` and garbage-collected with it. The residual risk is real: a compromised builder can plant a Secret in any namespace, which matters wherever something mounts Secrets by name. It cannot READ Secrets it was not pointed at, which is the half that would be worse. |

### E1 in detail

The build pod is where untrusted code executes, so it deserves its own statement.

It runs rootless BuildKit as uid 1000, `privileged: false`, no host namespaces, no device access, no
host mounts. It **does** have `allowPrivilegeEscalation: true` and `SETUID`/`SETGID`, and seccomp and
AppArmor unconfined — all four measured as necessary, none of them chosen for convenience
(ADR 0027).

The residual risk, stated plainly: **a setuid binary inside the build image can gain those two
capabilities within the container, and seccomp being unconfined widens the reachable kernel surface.**
That is not host root, and ADR 0001's blast radius — a container with host-level power — is not
reinstated. But builds share the node's kernel, and a kernel vulnerability reachable through an
unconfined seccomp profile is the realistic escape path.

Kubernetes user namespaces (`hostUsers: false`) would remove the need for escalation entirely and
map the container's root to an unprivileged host uid. ADR 0027 records that as the destination.

**Re-measured on 2026-08-21, on Kubernetes 1.36**, by a probe in the e2e suite
(`test/e2e/usernamespaces_test.go`) rather than by argument. The result, and it is more specific
than "it did not run":

- The **API server accepts** `hostUsers: false`. The feature gate is on; that half has moved since
  ADR 0027 was written.
- The **sandbox fails to start**, repeatably:

  ```
  FailedCreatePodSandBox: runc create failed: error during container init:
    error mounting "sysfs" to rootfs at "/sys": operation not permitted
  ```

**That is a property of the test environment, not a verdict on the feature.** The e2e runs on kind,
which is Kubernetes inside a Docker container, and a user namespace nested inside that container
cannot mount `sysfs`. A real node very possibly can. So this measurement rules out *shipping it on
the strength of CI*, and rules nothing else out.

The probe stays in the suite and reports on every run, so the day this starts working it says so
instead of waiting to be remembered. It skips rather than fails when unsupported — but fails loudly
if `hostUsers: false` is ever accepted and silently **ignored**, which would look like mitigation
and be none.

**If builds are hostile rather than merely untrusted, run them on dedicated nodes, or behind a
sandboxing runtime such as Kata or gVisor.** That is a cluster decision this project cannot make.

## Sequence: publishing a composition

The security-relevant ordering is that everything is resolved and hashed from the API server
*before* any bytes move, and that the tag-conflict check happens before any tag is written — and, on
`ImageBuild`, before the build Job exists at all.

```mermaid
sequenceDiagram
    autonumber
    participant T as Tenant
    participant K as API server
    participant C as Composer
    participant S as Flux source
    participant O as Origin (HTTP)
    participant R as Registry

    T->>K: apply ImageComposition
    K-->>C: watch event
    C->>K: get Secret (get only, own namespace)
    C->>S: read status.artifact
    Note over C,S: refused if generation != observedGeneration (ADR 0026)
    C->>C: InputHash(spec, digests, AssemblyVersion)

    alt hash unchanged and artifact present
        C->>R: HEAD manifest
        R-->>C: still there
        Note over C: converged, no bytes moved
    else rebuild needed
        C->>O: GET layer by URL
        O-->>C: bytes
        C->>C: verify against declared digest
        Note over C: refuse traversal, normalise mtime/uid/mode
        C->>C: assemble, deterministic
        C->>R: HEAD tag
        alt tag resolves to a different digest
            C->>K: Stalled: ImmutableTagConflict
            Note over C,K: refuses to overwrite, a human decides
        else
            C->>R: push manifest + blobs
            opt supplyChain enabled
                C->>R: SBOM + provenance as referrers
                C->>R: signature at sha256-<hex>.sig
                Note over C,R: skipped when status.attestations already matches
            end
            C->>K: status.artifact, history
        end
    end
```

## Sequence: running a build

```mermaid
sequenceDiagram
    autonumber
    participant T as Tenant
    participant K as API server
    participant B as Builder
    participant S as Flux source
    participant O as Origin (HTTP)
    participant J as Build pod
    participant R as Registry

    T->>K: apply ImageBuild
    K-->>B: watch event
    B->>S: read status.artifact
    B->>K: get Secret (resourceVersion only)
    B->>B: InputHash(spec, context digest, builder + frontend digests, secret identities)

    alt hash unchanged and artifact present
        Note over B: short-circuit, no Job created
    else
        B->>O: fetch Dockerfile (bounded, in memory)
        alt any FROM not pinned by digest
            B->>K: Stalled, no Job created
        else
            B->>K: copy push credential + registry CA into the tenant namespace
            Note over B,K: owned by the ImageBuild, collected with it (E6)
            B->>K: create Job (name derived from input hash)
            K-->>J: schedule pod
            Note over J: uid 1000, no token, drop ALL + SETUID/SETGID
            J->>O: fetch context tarball
            J->>J: BuildKit runs untrusted code
            J->>R: push image
            J-->>K: digest via termination message
            K-->>B: Job completed
            opt supplyChain enabled
                B->>R: sign the digest the Job reported
                Note over B,R: signed in the CONTROLLER, after the pod is gone —<br/>the key never shares a container with the built code
            end
            B->>K: status.artifact, history
        end
    end
```

Note step ordering: the pinned-`FROM` check happens **before** a Job exists, so an unpinned base
never reaches execution. The digest returns through the pod's termination message rather than by
granting the controller `pods/exec` or reading logs.

## Assumptions this model rests on

These are not mitigations. They are things assumed true, and each one is somebody else's job.

1. **`create` on `ImageComposition` / `ImageBuild` is a privilege.** A `ImageBuild` runs code, and
   both read every source in their own namespace. Grant them like you grant Pod creation.
2. **The registry's write path is reachable only with its credential** (S1), and network policy —
   not this code — restricts who can pull (I5).
3. **The registry is durable.** For `ImageBuild`, losing the store or status can mean a rebuild
   producing a digest that conflicts with an already-published tag under `onConflict: Fail`
   (ADR 0025).
4. **Nodes running builds are acceptable to share.** See E1.
5. **The cluster enforces `ResourceQuota`** where builds could otherwise exhaust nodes (D4).
6. **Whoever can read Secrets in the release namespace is trusted completely.** That set holds the
   registry password, the TLS private key and the cosign signing key, so it can push anything,
   impersonate the registry, and sign as this operator (S4, S6).
7. **Something verifies signatures at admission**, or signing changes nothing an attacker
   experiences. This project ships policies as examples only (`docs/examples/verify`); installing
   one is a separate decision (S5).
8. **The redis backing a clustered registry is persistent.** If it is not, retention deletes images
   live objects still reference (D9). Nothing here can check it.

## Known gaps, in priority order

| Gap | Threat | Note |
|---|---|---|
| The registry serves reads anonymously | I5 | Deliberate; a kubelet must pull without credentials. Now a zot policy rather than an unconditional property of this code, so it is changeable. Restricting *who* may pull is a NetworkPolicy question |
| `fetch.url` can still reach internal services by default | I6 | Link-local is always refused; the rest needs `--fetch-deny-private`, off by default because refusing in-cluster artifact servers would make the guard something people disable (ADR 0036) |
| The registry write credential crosses the pod network in the clear **by default** | I7 | `registry.tls.enabled=true` closes it (ADR 0038). Off by default because zot serves one scheme at a time, so enabling it also means updating the containerd drop-in on every node |
| A digest only a workload still needs is not refreshed | D8 | Retention follows live *objects*, not live *workloads*. Node-local images and Spegel absorb the running case; what is exposed is a pull onto a node that has never held it, once the registry has expired it — a rollback being the sharpest example. ADR 0019 |
| Attestations are off by default, and signing is inert without admission verification | R1 | Enabling `supplyChain` attaches an SBOM, provenance and a signature — but a signature changes nothing an attacker experiences until something refuses to run what it cannot verify. Example Kyverno and policy-controller policies: `docs/examples/verify` (ADR 0040) |
| `sourceRef.revision` is opt-in | T1 | Deliberate (ADR 0026), and now enforceable cluster-wide with `--require-pinned-sources`. Off by default: a spec that omits it still consumes whatever the source publishes |
| The `AssemblyVersion` bump is manual | T5 | The silent case is caught by a golden-digest test; what stays manual is deciding the change is intended and bumping the constant |
| A non-persistent redis makes retention delete live images | D9 | Only with read replicas, and the chart cannot detect it. The sharpest item on this list, because it fails silently and in the deleting direction -- and the likelihood rose when redis became the prerequisite of the availability feature |
| The signing key has no rotation story | S4 | Lost makes every artifact unverifiable; leaked makes every signature meaningless. Stated in ADR 0040, not solved |
| A signature attests provenance, not review | S5 | Any tenant who can create an object gets the operator's signature on it. Not a substitute for RBAC on the CRD |
| Only one pod can write the registry | D12/D14 | Measured, not assumed: two writers lose 2-4% of accepted tags. So pulls survive a drain and pushes do not, and push throughput does not scale |
| The builder can write a Secret into any namespace | E6 | Needed to copy the push credential and CA to a build's own namespace; it cannot read Secrets it was not pointed at |
| Build pods share the node kernel with seccomp unconfined | E1 | User namespaces are the destination (ADR 0027). Measured on 1.36: the API server accepts `hostUsers: false` and the sandbox fails to start under kind, which is a nested-container limitation rather than a verdict. An e2e probe re-measures every run |
| Retention depends on two numbers | D7 | **Closed for the bundled registry**: one chart renders both, so it refuses a margin below 24x and refuses refreshing disabled while a window is set. Still open for a registry you supply -- nothing here can read its policy |

## Reviewing this document

It is written against code, so it goes stale when the code moves. The claims most worth re-checking
are the RBAC verbs, the build pod's security context, and the namespace used for each reference —
those three carry most of the model. All are asserted by tests, so a change that invalidates a claim
here should also turn a test red.
