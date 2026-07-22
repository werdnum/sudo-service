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
- `imagePullSecrets` — registry-credential Secret references for pulling a private
  executor/init image (see below).
- `privileges.clusterAdmin` — the first explicit capability toggle.

The approve page is layered so the reviewer can't miss anything: prominent
callouts for the security-sensitive fields (namespace, privilege level, mounts,
env, init containers), the AI review aid, and — collapsible underneath — the
**full generated pod spec** (`displayPodTemplate`, the same `buildExecutorJob`
the controller runs) as ground truth. The callouts draw the eye; the full spec
guarantees no field is ever hidden by a curation gap. The AI aid reviews the same
ground-truth spec (with literal env values redacted, since it leaves the cluster).

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

### The executor VAP still scopes to the controller namespace

`validatingadmissionpolicy-executor.yaml` gates executor Jobs **in the controller
namespace** only (its `namespaceSelector`): "only the controller SA may create
one", plus structural guards (ownerRef, `backoffLimit: 0`, TTL, `restartPolicy:
Never`, no host namespaces, not privileged, not UID 0). Cross-namespace
mount-only Jobs run under a `default` SA and are **not** an escalation beyond what
any namespace tenant could already do, so they don't need VAP protection.

The one change: the match condition was broadened from "targets the executor SA"
to "targets the executor SA **or** carries the executor role label". Without this,
a non-cluster-admin in-namespace Job (`privileges.clusterAdmin: false`, which runs
under the `default` SA) would slip past the policy entirely. Keeping the SA branch
means a Job using the cluster-admin SA still can't dodge the policy by omitting the
label.

The Job policy alone is not sufficient for a reusable chart: a namespace editor
with `create pods` could otherwise create a raw Pod with
`serviceAccountName: sudo-service-executor-sa` and skip the Job admission check.
`validatingadmissionpolicy-executor-pod.yaml` closes that path. It permits a Pod
using the executor SA only when:

- the authenticated caller is the upstream Job controller (either its
  per-controller ServiceAccount or the shared kube-controller-manager identity),
- the Pod has exactly one controlling `batch/v1` Job ownerReference, and
- the stable Job name/controller UID labels agree with that ownerReference and
  the executor labels copied from the admitted Job are present.

The authenticated username is the important boundary here. OwnerReferences and
labels are caller-supplied on a direct Pod and therefore prove nothing by
themselves; they are checked only to keep the trusted Job-controller exception as
narrow as possible. Kubernetes may run controllers with individual ServiceAccount
credentials (`--use-service-account-credentials`) or one controller-manager
credential, so the policy accepts both upstream identities. A distribution that
uses a different Job-controller username must adapt this cluster-scoped policy as
part of installing the chart.

ValidatingAdmissionPolicy cannot dereference the Job. The chain is instead
composed from two admission decisions: only the sudo-service controller can create
an executor Job, and only the Kubernetes Job controller can turn such a Job into
a Pod. An actor allowed to impersonate either accepted system identity could forge
that second hop; such impersonation must remain control-plane-admin-only. The
chart deliberately does not grant it, does not grant the sudo-service controller
Pod creation, and does not broaden executor or controller RBAC for this policy.

Note that cross-namespace ownerReferences are not honoured by Kubernetes GC, so
the executor Job only carries an ownerRef to its SudoRequest in the controller
namespace; cross-namespace Jobs are reclaimed by `ttlSecondsAfterFinished`
instead. That TTL is floored at `ExecutorJobTTLFloor` (independent of the
requester's output-retention `ttlSecondsAfterApproval`) so a tiny requested TTL
can't let kube-controller-manager delete a finished Job before the reconciler
captures its pod logs.

`ttlSecondsAfterFinished` only fires once the Job *finishes*, so it does nothing
for a Job still running when its SudoRequest is deleted — and with no ownerRef
the deletion doesn't cascade either, leaving the pod (and its mounted Secrets/
PVCs) running indefinitely. So a cross-namespace SudoRequest carries an
`executor-cleanup` finalizer (added before its Job is created): on deletion the
controller stops the still-running Job before releasing the object. Same-
namespace requests keep relying on the ownerRef cascade and get no finalizer.

A Job can also be kept from finishing by a **mutating admission webhook that
injects a sidecar** (a service mesh, say): the sidecar outlives the executor
container, so the Job's `Succeeded`/`Failed` counts never advance and neither its
TTL nor the completion logic would ever fire. The reconciler therefore treats the
*executor container terminating* as completion (`executorContainerTerminated`),
independently of the Job counts, and deletes the Job afterwards to tear down the
lingering sidecar pod.

### Reading cross-namespace objects

The manager's cache is scoped to the controller namespace (to keep its RBAC
narrow), so the reconciler reads executor Jobs and their pods through the
uncached `APIReader`, not the cached client — otherwise a `spec.namespace` the
cache doesn't watch would read as empty/NotFound and the request could never
complete (or be misreported as Failed). Writes always go straight to the
apiserver, so only the read paths needed this.

### Curated subset enforced in Go, not the type system

The spec reuses the upstream `corev1` types (free DeepCopy, a one-line splice
into the pod), and `validateSpecExtras` is the single place that narrows them to
a reviewable, non-escalating subset:

- Volume sources are limited to `emptyDir`, `secret`, `configMap`,
  `persistentVolumeClaim`. `hostPath` is rejected, and so is `projected` — it can
  carry a `serviceAccountToken` source that would mint an API/cloud-capable token
  for the namespace default SA, bypassing the no-privileges guarantee.
- A `secret` volume/env/`envFrom` reference names a Secret but can't express its
  *type*, so the type-blind `validateSpecExtras` can't tell a credentials Secret
  from a `kubernetes.io/service-account-token` Secret. Mounting (or env-exposing)
  a real SA-token Secret is the same escalation as the rejected `projected`
  `serviceAccountToken` source by a different door. The controller therefore reads
  every referenced Secret's type at Job-creation time (it has the namespace and a
  reader there) and **rejects** service-account-token Secrets — but only for
  cross-namespace Jobs, since the controller-namespace executor is already
  cluster-admin and the mount grants it nothing new. This is the one allowlist
  guard that needs a cluster read; the rest are pure spec inspection.
- Env vars (literal values and `valueFrom` secret/configMap refs) and each init
  container's command and mounts are rendered on the approve page, so nothing
  executable is hidden from the reviewer.
- Init containers inherit the executor's locked-down securityContext **and** its
  bounded CPU/memory, so a requester init container can't run unbounded.
- Init containers may not set their own `securityContext`; the controller stamps
  the same locked-down profile as the executor container onto them.
- `imagePullSecrets` are an exception to the Secret-reference scrutiny above:
  unlike a volume/`env`/`envFrom` reference (whose bytes land inside the
  container), a pull secret is consumed only by the **kubelet** to authenticate
  to the registry and is never projected into the pod. So it can't smuggle in API
  credentials the way a mounted service-account-token Secret would, and it needs
  no allowlist or SA-token rejection — the controller only checks each reference
  has a `name`, and surfaces the names to the reviewer. (A bad/foreign pull secret
  at worst makes the image fail to pull, which the start deadline already catches.)

Both submission paths run it: the HTTP API rejects a bad spec with `400`, and a
CRD-created one is moved straight to `Denied` (`deniedBy=spec-validation`) before
any approval push — exactly like the existing shell-syntax check.

### Writable /tmp and HOME by default

The executor (and every init container) runs with `readOnlyRootFilesystem: true`,
so `/` — including `/tmp` and the image's home directory — is read-only. That is
the right security posture, but on its own it is a footgun: a huge fraction of
ordinary commands write a scratch file under `/tmp` or a dotfile/cache under
`$HOME` and fail with `EROFS`, and every requester would re-discover this the hard
way and have to hand-roll the same two `emptyDir` mounts.

So `buildExecutorJob` splices a writable `emptyDir` into each container
at `/tmp` and at `/home/sudo-service`, and points `HOME` at the latter (an
arbitrary UID has no `/etc/passwd` home, so without this `$HOME` resolves to a
read-only default). These are controller-owned, like the stdin volume: their names
(`sudo-service-tmp`, `sudo-service-home`) are reserved by `validateSpecExtras`, and
they show up in the ground-truth pod spec on the approve page. Each default steps
aside the moment the requester is managing that area, so the controller never nests
a volume over a requester mount or overrides a requester-set value:

- `/tmp` is suppressed if the requester mounts anything at `/tmp` *or below it* (an
  emptyDir at `/tmp` underneath a requester Secret at `/tmp/creds` is a nested mount
  Kubernetes handles unreliably).
- `HOME` is left untouched if the requester sets `HOME` directly **or** supplies any
  `envFrom` (which could carry `HOME` — an explicit `env` entry the controller
  appended would override it, since `env` beats `envFrom`). If the requester has
  already mounted a writable volume *exactly* at `/home/sudo-service`, `HOME` is
  pointed there without adding a second volume.

So a request that wants `/tmp` backed by a large PVC, or a specific `HOME`, is never
fought by the default. The default scratch emptyDirs have no sudo-service-specific
size limit; aggregate disk protection belongs to the cluster's ordinary quota and
eviction policy. A requester can still provide an explicit `sizeLimit` when the
operation itself calls for one.

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

### Accepted limitations: executing in a namespace you don't trust

A cross-namespace executor Job runs under the target namespace's `default`
ServiceAccount and (necessarily) shares that namespace with whatever tenants live
there. A tenant who holds Pod/Secret RBAC in that namespace can interfere with our
objects in ways the controller can't fully prevent:

- **Output integrity.** `getJobPod` matches the executor pod by its `job-name`
  label and the Job's controller ownerRef (UID). In the controller namespace that
  is authoritative (tenants can't create Pods there and the executor VAP gates it).
  In a target namespace, `job.UID` is readable and an ownerRef is forgeable, so a
  tenant could present a spoofed pod and have its logs/exit code recorded as the
  request's result.
- **Adoption crash window.** The executor Job's name is an unguessable minted
  token and a pre-existing Job at that name is failed closed, but a same-name
  collision inside a narrow controller-crash window is theoretically possible.
- **stdin swap.** The stdin payload Secret lives in the target namespace; a tenant
  with Secret write there could delete and recreate it between our create and the
  kubelet's projection, so the command could run with stdin other than what was
  approved.

These are all **bounded by the fact that the cross-namespace Job is
non-privileged** — it can do nothing a tenant of that namespace couldn't already
do directly — so none is an escalation; they are integrity/audit edges, not new
capabilities. (The SA-token mount above *would* have been an escalation, which is
why it is actually rejected rather than merely documented.) They are accepted as
documented residuals. The proper structural fix is a **per-namespace opt-in**: gate
executor Jobs in the target namespace with the same ValidatingAdmissionPolicy the
controller namespace uses, so only namespaces the operator has vouched for (and
locked down) can host cross-namespace executors. That is future work, not wired up
here.

## Future direction

`privileges` is the extension point for the other capabilities discussed:
`privileged`, `runAsRoot`/UID 0, `hostPath`, `hostNetwork`/`hostPID`. Each will
default off, be surfaced individually on the approve page, and relax the
corresponding `validateSpecExtras` guard (and, for the host-namespace / privileged
toggles, require loosening the executor VAP's per-capability denials — which can
only be done by moving that gating into the controller, since CEL can't read the
SudoRequest's approval state across objects). They are intentionally not wired up
yet.
