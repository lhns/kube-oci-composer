# 36. SSRF on `fetch` URLs: block the metadata endpoint, make the rest opt-in

Date: 2026-08-21

## Status

Accepted. First record of a decision that until now existed only in a commit message.

## Context

`spec.layers[].fetch.url` is a spec field, so anyone with `create` on an `ImageComposition` chooses
an address the controller sends a `GET` to, from the controller's network position inside the
cluster. That is threat I6 in `docs/threat-model.md`.

**The decision was made once and never written down.** Commit `061ad6c` records I6 as "considered
and declined" in its message, while the STRIDE table it edited continued to say `NOT mitigated`. So
the project had a decision nobody could find and a document contradicting it — which is worse than
either an open gap or a closed one, because there was no way to tell which it was.

**What digest verification does and does not cover.** A fetched response only becomes a layer if it
matches the declared digest, so an attacker cannot use this to *smuggle content in*. Exfiltration
is likewise limited: the response is not readable from the object's status.

What it does not cover is the request itself. A blind `GET` is enough to:

- reach `169.254.169.254` and ask for cloud credentials — which every major provider serves to any
  client that asks, over plain HTTP, with no authentication,
- reach an internal service that acts on a `GET`,
- probe reachability and ports through response timing.

**The obvious mitigation is wrong here.** Blocking all private ranges — the standard SSRF answer —
would refuse this project's most ordinary layer source: an artifact server on a private address in
the same cluster. RFC1918 is not a smell in this context, it is the normal case. A guard that
refuses legitimate configurations gets turned off, and then it protects nothing.

## Decision

**Split the two by how defensible each is.**

- **Link-local is always blocked** — `169.254.0.0/16` and `fe80::/10`, with no flag to permit it.
  This is where cloud metadata lives on AWS, GCP, Azure and Hetzner alike, and no legitimate layer
  source is there. The unspecified address is blocked with it.
- **Everything else private is blocked only on request**, via `--fetch-deny-private`: RFC1918,
  loopback, unique-local, and `100.64.0.0/10` (CGNAT, which `net.IP.IsPrivate` does not cover and
  where several managed providers put their node networks).

**Enforced in the dialer, not by parsing the URL.** The check runs in `net.Dialer.Control`, after
the address is resolved and immediately before `connect(2)`. That is what makes it hold: a hostname
resolving to `169.254.169.254`, an HTTP redirect to it, and a DNS rebind that answers differently
the second time all arrive at the dialer, and none of them is visible in the URL string. A
check-then-connect design has a window between deciding an address is safe and using it, which is
exactly the window a rebind needs.

**Installed in `NewFetcher`, not offered as an option.** A fetcher built without the guard is one
an attacker-supplied URL can point at a metadata endpoint, and that must not be something a caller
has to remember.

**The refusal is its own error type.** `ErrBlockedAddress` names the host, the resolved IP and the
reason, because "connection refused" for a blocked metadata endpoint is the kind of message that
sends someone debugging their network for an hour.

## Consequences

**I6 moves from "NOT mitigated" to "the credential-stealing case is closed; the rest is opt-in."**
That is an honest description rather than a claim of mitigation: an operator running
`--fetch-deny-private=false`, which is the default, still has a controller that will `GET` an
internal address a tenant names.

**Anyone whose layer sources are external can and should set `--fetch-deny-private`.** It is one
value, and the cluster it protects is the one where a tenant can create objects.

**The default is a deliberate hole, and naming it is the point.** Choosing safety-by-default here
would mean refusing in-cluster artifact servers, which are a supported and common source. The
project takes the position that a guard people keep is worth more than one they disable — and
records that position here so the next person does not have to infer it from a commit message.

**Falsified, per the standing rule.** Removing the link-local branch makes the test fail — and
takes 66 seconds to do it, because without the guard the test actually attempts the connections it
exists to prevent.
