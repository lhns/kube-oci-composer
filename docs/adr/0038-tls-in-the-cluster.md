# 38. TLS in the cluster, and who has to trust what

Date: 2026-08-21

## Status

Accepted. Closes threat **I7**. The first record of any TLS decision in this project — see the
Context for why there was never one to supersede.

## Context

**Threat I7: the registry write credential crosses the pod network in the clear.** zot terminates
no TLS and authenticates writes with HTTP Basic, so the generated password travels base64-encoded
in a header, readable by anything positioned to watch it — a compromised CNI, a node, a container
with `NET_RAW` on the path — and the capture leaves no trace in any log. That credential is the
whole of the "only the controllers can push into the composer's registry" guarantee.

Pulls are anonymous by design (threat I5), so **only the write path is exposed**. It is also the
path that matters.

**There has never been TLS in this project, and it is worth saying plainly**, because the history
suggests otherwise. A search of all 168 commits for `crypto/tls`, `ListenAndServeTLS`, `x509`,
`CreateCertificate` and `selfsigned` returns nothing; the deleted `internal/serve/server.go` called
plain `ListenAndServe`. What existed was an Ingress template passing through a **user-supplied**
`tls.secretName`. The chart has never created a certificate.

## Decision

**zot terminates TLS itself, opt-in, with three sources for the certificate.**

| `registry.tls.mode` | Who owns the certificate | Renewal |
|---|---|---|
| `selfSigned` | the chart generates a CA and a leaf | manual, and the chart refuses to render near expiry |
| `certManager` | the chart renders a `Certificate` | cert-manager |
| `secret` | you supply a `kubernetes.io/tls` Secret | you |

**Default off, and that is not timidity.** zot has one listener and cannot serve HTTP and HTTPS at
once, so turning TLS on invalidates the containerd drop-in on **every node**: each `hosts.toml`
moves from `http://` to `https://` and, in `selfSigned` mode, has to learn the CA. Shipping that as
a default would mean a release whose flagship install produces images nothing can pull. Anyone
terminating TLS at an ingress instead is unaffected — the node talks to the ingress, which already
has a certificate it trusts.

**The CA is additive, never replacing, and it applies to every registry the controller talks to.**
`recon.Transport` starts from `x509.SystemCertPool` and appends; if the system pool cannot be read
it fails startup rather than falling back to an empty one. The narrow alternative — scoping the CA
to the operator's own registry — was rejected, and the decisive reason is that a composition
layered on an `ImageBuild`'s output pulls its **base** from the bundled registry. Scoping would
produce pushes that succeed and base pulls that fail with `x509`, which reads as a registry bug
rather than a configuration one. The exposure it would have bought is nil anyway: whoever can
present a certificate signed by that CA already holds the registry password, from the same Secret,
in the same namespace.

**Build Jobs get the CA through the mechanism that already exists for the push credential.** The Job
runs in the tenant's namespace and a pod mounts only from its own, so the CA is copied there, owned
by the `ImageBuild`, and garbage-collected with it. A **separate** Secret rather than an extra key
on the copied credential: `pushSecretFor` returns early when the object has its own `secretRef`,
and such an object may still legitimately push into the operator's registry — riding the CA on the
copy would give it no CA and a TLS failure that looks nothing like a credential problem.

Making rootless BuildKit use it needs `SSL_CERT_FILE`, because `/etc/buildkit/certs/<host>` is
ignored in rootless (moby/buildkit#6406). And `SSL_CERT_FILE` **replaces** Go's system pool rather
than adding to it, so the build script merges the image's own bundle with the mounted CA first —
otherwise every `FROM alpine` and every frontend fetch from Docker Hub stops verifying.

## Consequences

**The probes cost nothing.** The kubelet's HTTPS prober sets `InsecureSkipVerify` and does no
hostname verification — it must, since it probes the pod IP, which is in no SAN list. Only
`scheme: HTTPS` is needed. The handshake still has to complete, so a certificate zot could not load
still fails the probe, which is the part worth keeping.

**Removing the Service from the insecure list is load-bearing, and its failure mode is silent.**
The same list becomes BuildKit's `registry.insecure=true`, which means allow plaintext **and** skip
verification. Leaving the Service name on it with TLS enabled would make the controllers fail loudly
while builds kept pushing the Basic header in the clear — the chart would look like it had closed
I7 while it had not. There is a test at the rendered-flag layer and another at the rendered-Job
layer, because one of them alone would not have caught it.

**Self-signed mode does not renew, and the failure is deletion rather than an outage.** The
`lookup`-reuse pattern that keeps the password stable will just as happily re-emit a certificate
that expired last week, and sprig has no PEM parser to notice. So the expiry is written into the
Secret at generation and compared on every later render, and the chart **fails** inside
`failWithinDays`. It has to be a failure rather than a warning: an expired certificate stops the
retention refresh, and a refresher that stops running is what lets a registry reclaim images live
objects still reference (ADR 0031). One window later, silently, in the deleting direction.

**cert-manager has a quieter version of the same problem, and there is no clean in-chart fix.**
cert-manager renews the Secret in place, the kubelet updates the projected volume, and zot keeps
serving the old certificate — it reads certs once at startup, which is why the config checksum
annotation exists at all. Nothing here can compute a checksum over a Secret the chart does not own.
The mitigation is a generous `renewBefore` and documentation; a hard dependency on Reloader was
rejected.

**No `checksum/tls` annotation on the registry pod**, deliberately, despite the `checksum/config`
precedent sitting next to it. Computing it would re-run `genCA` and produce a different value on
every render, rolling all three workloads on every no-op `helm upgrade`.

**`http.tls`, never `http.tls.cacert`.** One word apart and opposite meanings: the second makes zot
demand a client certificate from every caller, including the kubelet, which has none — every pull
in the cluster stops. Full mutual TLS would mean issuing client certificates to every pulling
workload, and is not implemented.

## Alternatives rejected

**Terminate at an ingress only, leaving in-cluster traffic on HTTP.** Much less work, and it leaves
I7 open while looking like it closed it — the credential still crosses the pod network in the clear
between the controllers and the registry, which is where it actually travels.

**Default `tls.enabled: true`.** Breaks every documented pull path in one step.

**Generate the CA in more than one template.** `genCA` is not deterministic and `lookup` returns
nothing on a first install, so two call sites produce two unrelated CAs, a render that looks
correct, and `x509: certificate signed by unknown authority` on install — on the path with the
least scrutiny. Everything is computed once, in `registry-tls.yaml`, into one variable, and a test
asserts the Secret's `ca.crt` and the ConfigMap's are byte-identical.
