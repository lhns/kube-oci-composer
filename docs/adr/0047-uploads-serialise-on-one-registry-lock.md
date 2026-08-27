# 47. Uploads serialise on one registry lock, and the timeout counts the waiting

## Status

Accepted.

## Context

A build succeeded, then died at `exporting to image`, retrying one blob four times over five minutes:

```
Put ".../blobs/uploads/<uuid>?digest=sha256:…": write tcp …:5000: use of closed network connection
```

### The chain

`ImageStore.lock` is a **single `sync.RWMutex` for the whole store** — one per `rootDirectory`, so
registry-wide across every repository. Two things take it for writing:

- `FinishBlobUpload` holds it across `DedupeBlob` **and** the blob move.
- `InitRepo` — and `PutBlobChunk` calls it as its **first action, before reading a byte of the body**.

So a second upload, in any repository, blocks on that lock before transferring anything. Go's
`http.Server.ReadTimeout` bounds the **whole request including the body**, and it is running the
entire time the handler is parked on the mutex. At 60s the server closes the connection; zot logs
`i/o timeout` only when it finally acquires the lock and reads a body that is gone.

**It is self-sustaining.** A failed push restarts from zero, so each retry is another full upload
ending in another `FinishBlobUpload` holding the lock, which times out the next queued push.

### Measured

`test/spike/contention_test.go`, against zot v2.1.20 on fast local storage with 1MB blobs — the
least favourable conditions for showing it:

|            | single upload | twenty concurrent (min–max) |
|------------|---------------|------------------------------|
| dedupe on  | 0.366s        | **3.94s – 6.06s**            |
| dedupe off | 0.341s        | **1.01s – 3.93s**            |

Twenty *small* pushes already consume a third of a 60s budget. A large layer on slower storage
passes it having transferred nothing.

Dedupe is a real lever and a **partial** one: removing it cuts the worst case by about a third, and
twenty concurrent pushes still degrade elevenfold, because `InitRepo` takes the same lock.

### Two diagnoses that were wrong, and what killed them

| observation | claimed | actually |
|---|---|---|
| identical via pod-IP and Service | a CNI or proxy capping request duration | the network was never involved |
| 1.2 MB/s | "the suspicious number", a storage fault | 60s of lock-wait divided by whatever moved |
| 44m CPU, 50Mi memory | unexplained | a blocked goroutine does no work |
| 60.0–60.3s, three times | a rate would vary | a fixed deadline, ticking during lock-wait |
| 39.6 MB/s when idle | contradicted the framing | nothing was ever slow |

The throughput figure was derived from a wall-clock that was almost entirely queueing. Worth
recording because the same mistake is available to anyone who measures a rate here.

`Config.GetHTTPReadTimeout()` returning 0 when nothing set it makes the getter actively misleading:
the default is injected by zot's CLI, in another package, before the server is built.

**The TLS asymmetry is a red herring.** BuildKit's error names an `https://` URL while zot serves
plaintext. That is containerd's error formatting, not the protocol — proven by zot's own route
handler running, which a TLS ClientHello to a plaintext server could never reach, and by plain-HTTP
`curl` reproducing the reset at the same 60.1s.

## Decision

**The chart states `http.readTimeout`, generously, always.** Unset, zot's CLI injects 60s. This is
the load-bearing fix: a queued push that waits and then succeeds never retries from zero, so nothing
feeds the queue. **Contention becomes latency instead of failure.** An empty value is refused at
template time, because rendering no key silently restores the 60s.

**`storage.dedupe` is exposed, defaulting to `true`.** That is what zot does anyway. Turning it off
shortens the critical section; it trades disk for latency, and that is the operator's call rather
than a default to change under them.

**No cap on concurrent builds.** It was considered and rejected: it would throttle every build to
protect one shared resource, including the many that push small layers and were never the problem.
With a generous timeout the failure it guards against does not occur.

## Consequences

**A long `readTimeout` makes a dead connection expensive** — a goroutine and an incomplete
`.uploads/` entry held until it expires. The sensible bound is on a *stalled* transfer, which Go's
`ReadTimeout` cannot express, so the value is exposed rather than hardcoded.

**The default configuration still serialises.** Pushes queue rather than fail, which is the change
that matters, but throughput under concurrent large pushes stays poor until dedupe is turned off.
The docs say so rather than implying the problem is gone.

**Chunked upload is not available to us.** It is a *client* decision, and neither push path makes
it: `ImageBuild` pushes via `buildctl`, and `ImageComposition` via go-containerregistry, where
`Content-Range` appears zero times in `pkg/v1/remote`. zot implementing `PutBlobChunk` is necessary
but not sufficient.

**If this recurs, the next lever is concurrency.** Nothing currently bounds in-flight builds:
`MaxConcurrentReconciles` is unset, and it would only serialise Job *creation* anyway, since
`startBuild` returns as soon as the Job exists. Named here so it is not rediscovered.
