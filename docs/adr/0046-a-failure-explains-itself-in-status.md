# 46. A failure explains itself in status

## Status

Accepted. Reverses a decision recorded only as a field comment.

## Context

Finding out why an `ImageBuild` failed took five steps, and the third usually failed:

1. `kubectl get imagebuild` — `Ready=False`, no reason shown.
2. The message ended in ``see `kubectl -n ns logs <pod> -c build` ``.
3. That pod was usually gone: `error: pods "…-d7bcc" not found`.
4. So: the controller log, which repeats the failure every few minutes with a twelve-line
   stacktrace attached, leaving you grepping JSON for one line.
5. And you had to know whether to ask for `-c fetch-context` or `-c build`.

**The pointer named a resource with a shorter lifetime than the failure it described.** `observeJob`
deletes the failed Job when the retry falls due — deliberately, to avoid a hot loop — and the pod
goes with it.

### Almost all of the machinery was already right

`observeJob` keeps the failed Job through its backoff and captures the detail on FIRST observation
*"while it is still there"*, storing it in `status.lastAttempt.message` precisely so later passes do
not re-read a collected pod. A Warning Event carries the same text. `recon.Truncate` exists for this
class of limit.

It was all fed an empty string, because of one line:

```go
TerminationMessagePolicy: corev1.TerminationMessageReadFile,   // the build container
```

`ReadFile` takes the termination message only from `/dev/termination-log`, which `buildctl` never
writes. The **fetcher** has had `FallbackToLogsOnError` since ADR 0044's work — which is exactly why
*its* failures explained themselves ("staging the download: … permission denied" reached status
verbatim) while the build container's did not. Nothing defended `ReadFile`; it was the explicit
default.

## Decision

**The build container gets `FallbackToLogsOnError` too, and the message leads with the cause.**

The kubelet copies the log tail into `terminationMessage` on a non-zero exit. That matters for more
than convenience: it means this costs **no `pods/log` grant**, no controller-side log reading and no
extra API calls — which is what makes it cheap enough to be the default rather than an opt-in knob.
It also answers step 5, since whichever container failed contributes its own tail.

**The cause comes first, the mechanism after.** It used to read `build failed: Job has reached the
specified backoff limit: container "build" exited 1 (Error): <cause>`, so every truncation ate the
one part worth reading.

**Only the cause is trimmed, and from the front.** `BuildAttempt.Message` is `MaxLength=4096` and a
termination message can be ~4KB by itself. An over-long value does not truncate — the API server
**rejects the status write**, losing the failure entirely rather than shortening it. `TruncateTail`
keeps the end, because a build's error is its last lines.

**Stacktraces come off `Error` in both binaries** (`opts.Zap`). Not by lowering the level: a failing
build is worth an error line, and the severity was never what was wrong. A reconcile error's Go
stack is not the useful part — the error is already wrapped with the context that matters — so the
trace is removed at the logger, for every call site, while panics keep theirs.

## Consequences

**This reverses `BuildAttempt.PodName`'s comment**, which read *"Logs are not copied into status, so
this is what `kubectl logs` needs"*. They are now, in bounded form. The pod name stays as data for
the full log, and the message says plainly that it lasts only as long as the pod does.

**A user's build output now lands in status and in an Event.** It is attacker-influenced content —
whatever a Dockerfile prints — stored on an object and echoed to anything watching events. Nothing
executes it, and it is already readable by anyone who can reach the pod, but it reaches a new place
and truncation is the only thing bounding it. Recorded in the threat model rather than left implicit.

**The tail may be all you get.** 4096 bytes is roughly the last fifty lines. A failure whose cause
scrolled past that still needs the pod, which is why the hint remains.

[ADR 0026](0026-a-source-artifact-can-lag-its-own-spec.md) is the nearest relative — *record in
status what you will need to diagnose* — but it decided a different question, about trusting a
source's own status, and is cited here only for the shape.
