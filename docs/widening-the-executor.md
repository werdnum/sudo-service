# Widening the executor: structured pod fields & cross-namespace jobs

## Problem

The original interface ran every request as `sh -c <command>` in a single,
fixed executor pod (one container, the default image, a locked-down
securityContext, no volumes) under a cluster-admin ServiceAccount in the
`sudo-service` namespace. That is fine for one-liners, but a large class of real
requests — "recover this volume", "run this multi-step script that needs a
credential file and a PVC" — can't be expressed in that shape. The only way to
get volume mounts, a credential Secret, an init container, or a non-default image
was to use the executor's cluster-admin `kubectl` to **create a second Job** that
had the right shape: a job to make a job. That payload was a full manifest
smuggled through the single `command` string, with two or three layers of nested
shell/YAML quoting — exactly the escaping that kept going wrong.

## What changed

`SudoRequestSpec` gained a curated set of pod fields and an explicit privilege
block, so the request **is** the pod that runs instead of a `kubectl` that
creates one:

- `namespace` — the namespace the executor Job runs in (default `sudo-service`).
- `stdin` — payload fed to the command's stdin (see below).
- `env`, `envFrom`, `volumes`, `volumeMounts`, `initContainers` — the upstream
  Kubernetes shapes, narrowed to a reviewable allowlist in Go.
- `privileges.clusterAdmin` — the first explicit capability toggle.

The approve page now renders the namespace, the privilege level, and every mount
/ init container, so the human approves the actual action rather than inferring
it from a command string.

## Design decisions

### cluster-admin is exclusive to the controller namespace

The two needs that drove this are orthogonal and are kept separate:

- **API-as-root** uses the single cluster-admin executor SA, which lives only in
  `sudo-service`. `kubectl exec/patch/delete` already reach other namespaces from
  there, so cluster-admin never needs to physically run elsewhere.
- **Mounting another namespace's Secrets/PVCs as files** is the only thing that
  requires running in that namespace (pods can't mount cross-namespace), and it
  needs **no** API privilege. Those Jobs run under the target namespace's
  built-in `default` ServiceAccount with `automountServiceAccountToken: false`.

So we never need a privileged SA outside `sudo-service`, and `clusterAdmin` is
rejected when combined with a non-default `namespace` (see `validateSpecExtras`).
`clusterAdmin` defaults to **true** in the controller namespace, preserving the
historical "every request is fully privileged" behaviour; it is unavailable
(and defaults off) elsewhere. The rare request that needs both cluster-admin and
a foreign-namespace mount is consciously out of scope — decompose it or fall back
to a job-that-makes-a-job.

### The executor VAP is deliberately unchanged

`validatingadmissionpolicy-executor.yaml` still gates only **same-namespace,
cluster-admin** executor Jobs: "only the controller SA may create a Job that runs
as `sudo-service-executor-sa`", plus structural guards (ownerRef, `backoffLimit:
0`, TTL, `restartPolicy: Never`, no host namespaces, not privileged, not UID 0).
Cross-namespace mount-only Jobs run under a `default` SA and are **not** an
escalation beyond what any namespace tenant could already do, so they don't need
VAP protection. The VAP's job is to protect the cluster-admin SA, and that scope
is unchanged.

Note that cross-namespace ownerReferences are not honoured by Kubernetes GC, so
the executor Job only carries an ownerRef to its SudoRequest in the controller
namespace; cross-namespace Jobs are reclaimed by `ttlSecondsAfterFinished`
instead.

### Curated subset enforced in Go, not the type system

The spec reuses the upstream `corev1` types (free DeepCopy, a one-line splice
into the pod), and `validateSpecExtras` is the single place that narrows them to
a reviewable, non-escalating subset:

- Volume sources are limited to `emptyDir`, `secret`, `configMap`,
  `persistentVolumeClaim`, `projected`. `hostPath` (and anything else) is
  rejected.
- Init containers may not set their own `securityContext`; the controller stamps
  the same locked-down profile as the executor container onto them.

Both submission paths run it: the HTTP API rejects a bad spec with `400`, and a
CRD-created one is moved straight to `Denied` (`deniedBy=spec-validation`) before
any approval push — exactly like the existing shell-syntax check.

### stdin without escaping

`stdin` is materialised into a short-TTL Secret (owned by the executor Job, so it
is GC'd with it) and mounted into the pod. The command is passed to the shell as
a **positional parameter** (`sh -c "$1"`), never interpolated into the script
text, and an outer shell redirects fd 0 from the mounted payload. So a manifest
piped to `kubectl apply -f -` travels as literal bytes with zero shell quoting.

### Auto-approve

The auto-approve allowlist only reasons about `command` + `image`, so any request
that uses the widened fields or a privilege toggle (`hasSpecExtras`) always
requires a human.

## Future direction

`privileges` is the extension point for the other capabilities discussed:
`privileged`, `runAsRoot`/UID 0, `hostPath`, `hostNetwork`/`hostPID`. Each will
default off, be surfaced individually on the approve page, and relax the
corresponding `validateSpecExtras` guard (and, for the host-namespace / privileged
toggles, require loosening the executor VAP's per-capability denials — which can
only be done by moving that gating into the controller, since CEL can't read the
SudoRequest's approval state across objects). They are intentionally not wired up
yet.
