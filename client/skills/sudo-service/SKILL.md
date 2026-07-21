---
name: sudo-service
description: Request a human-approved privileged Kubernetes action via the sudo-service (https://sudo.andrewgarrett.dev). Use whenever you're running in an agent that lacks RBAC to mutate cluster state but needs to read a Secret, restart a Pod, patch a Deployment, exec into a container, etc.; or when you want a clear audit trail with a human checkpoint.
---

# sudo-service

`sudo-service` accepts a `SudoRequest` Custom Resource (or HTTP POST), sends a
Pushover push to the admin, and — only on approval — runs the requested command
under `cluster-admin` and returns the output to the requester. The hard barrier
is the human approval click; nothing happens until they tap it.

Source of truth for the design and controller: the `werdnum/sudo-service` repo
(`README.md`, controller `*.go`, Helm chart `charts/sudo-service/`). The
requester CLI ships from `client/cli/sudo-service` in that same repo. Cluster
wiring (requester RBAC, projected SA token, SealedSecrets) lives in
`werdnum/kube-config` under `kubernetes/manifests/workloads/`.

The CLI requires the pinned PyYAML version in `client/cli/requirements.txt` so
YAML request files are parsed with `yaml.safe_load`, not an approximate parser.

## When to use this skill

- An alert points at a private Secret you need to read.
- You want to `kubectl rollout restart` / `patch` / `delete` something to
  remediate.
- You want to `kubectl exec` into a Pod for a one-off diagnostic.
- Any time you'd ordinarily ask the human "can you run X for me?" in chat.

Don't use it for read-only operations you can already do (`kubectl get/list/watch`).

## Setup the requester pod needs (one-time)

**RBAC.** HTTP submission requires `create` on the virtual
`sudorequests/submit` subresource in the `sudo-service` namespace. Direct CR
submission separately requires `create` on
`sudorequests.sudo.andrewgarrett.dev`; neither permission implies the other.
The operator must grant the path the requester uses. `get`/`list`/`watch` is
intentionally *not* granted; state reads go through the controller HTTP API.

**Audience-bound SA token.** Mount a projected SA token volume on the requester
pod so the controller's TokenReview check passes:

```yaml
volumes:
  - name: sudo-service-token
    projected:
      sources:
        - serviceAccountToken:
            audience: sudo-service.andrewgarrett.dev
            expirationSeconds: 3600
            path: token
volumeMounts:
  - name: sudo-service-token
    mountPath: /var/run/secrets/sudo-service
    readOnly: true
```

The default apiserver-audience SA token will be rejected with 401.

## The flow

If the `sudo-service` CLI is available, prefer it for one-off requests:

```bash
sudo-service \
  --reason "One sentence: WHY you need this, what alert/task it's for." \
  -- kubectl get nodes
```

For anything richer than a command, use `--request-file` with a complete YAML
or JSON HTTP request body. Do not put a manifest, heredoc, encoded script, or
nested Job/Pod definition in `command`. Use `--stdin-file` for a separate
literal input payload; it can be combined with `--request-file` when the request
file does not already set `stdin`.

```bash
sudo-service --request-file /tmp/request.yaml --preview
sudo-service --request-file /tmp/apply-request.yaml --stdin-file /tmp/manifest.yaml
```

`--preview` prints normalized JSON to stderr immediately before submission.
All request fields must come from the request file: mixing it with `--reason`,
`--command`, command arguments, image/profile, namespace, privilege, TTL, or image-pull
Secret flags is rejected rather than applying surprising precedence.

It creates the request through the controller HTTP API, polls
`/requests/{uid}`, prints progress on stderr, and writes the command output
from `/requests/{uid}/output` to stdout when the command executes. For shell
pipelines or JSONPath braces, pass a single command string:

```bash
sudo-service \
  --reason "Confirming PSQL credentials for keep-backend after auth alert" \
  --command 'kubectl get secret keep-postgres-credentials -n keep -o jsonpath={.data.password}'
```

The CLI waits for up to 12 hours by default. That is only a client-side wait;
it does not terminate an executor that is already running. Use `--timeout 0`
when the caller should wait indefinitely, or `--detach` to print the request UID
and return without polling. Detach is appropriate when another agent turn will
resume the request; retain the printed UID and query that exact request later.

### 1. Draft the request

Pick the smallest, most legible command. The human sees the verbatim
`command` + `reason` + executor profile and exact resolved image on the approve
page, so be surgical — e.g.
`kubectl get secret foo -n bar -o jsonpath={.data.x}`, not a kitchen-sink
shell pipeline.

```yaml
reason: "One sentence: WHY you need this, what alert/task it's for."
command: "exact shell command, single string"
# profile: kubectl                   # optional; server-resolved default
# image: example/tool@sha256:...     # arbitrary image; mutually exclusive with profile
# ttlSecondsAfterApproval: 3600      # output retention, default/max 3600s
# --- optional, for more than a one-liner (see "Richer jobs" below) ---
# namespace: seaweedfs               # run the Job here to mount this ns's Secrets/PVCs
# stdin: |                            # fed to the command's stdin, no shell quoting
#   ...multi-line payload...
# env: [{name: FOO, value: bar}]
# envFrom: [{secretRef: {name: some-secret}}]
# volumes: [...]                      # emptyDir/secret/configMap/persistentVolumeClaim only
# volumeMounts: [...]
# initContainers: [...]
# imagePullSecrets: [{name: registry-creds}]  # pull a private image; never exposed to the command
# privileges: {clusterAdmin: true}    # default true in sudo-service ns, unavailable elsewhere
```

If no convenient image contains an uncommon tool combination, remember that the
CLI can build an arm64 Nixery image with repeated `--tool` flags. For example,
`--tool openssh --tool ansible --tool kubectl --tool opentofu` submits the normal
raw image
`nixery.dev/arm64/shell/ansible/kubectl/openssh/opentofu`; there is no special
server-side Nixery profile. Prefer this to spending time searching for a bespoke
image. Use `--image` directly for another architecture or a manually composed
Nixery URL. Nixery images may pull slowly the first time.

OpenSSH is unusual because it requires a passwd entry for the executor's numeric
UID. If it reports `No user exists for uid 1000`, use the documented structured
init-container and `/etc/passwd` subPath workaround in `client/README.md`; do not
infer that all Nixery tools require special handling.

The HTTP API stamps the authenticated requester and the request file must not
contain `requester`. In the lower-level direct-CRD fallback below, a
`ValidatingAdmissionPolicy` enforces `spec.requester == request.userInfo.username`,
so don't put another ServiceAccount in there.

### 2. Submit and capture the request uid

Normally the CLI submits the request and retains its uid automatically. Use
`sudo-service --request-file request.yaml` and let it wait for the terminal
state. For asynchronous work, use
`REQUEST_UID=$(sudo-service --request-file request.yaml --detach --quiet)`.
The lower-level direct-CRD flow below is only for environments where the CLI is
unavailable.

The requester SA has `create` only — no get/list/watch. So you MUST capture
the uid at submission time; you can't look it up later by name.

Store it in a variable like `REQUEST_UID` — **not** `UID`, which is a read-only
shell variable in bash (`UID=...` fails with `bash: UID: readonly variable`).

```bash
REQUEST_UID=$(kubectl create -f - -o jsonpath='{.metadata.uid}' <<'YAML'
apiVersion: sudo.andrewgarrett.dev/v1alpha1
kind: SudoRequest
metadata:
  generateName: agent-
  namespace: sudo-service
spec:
  requester: system:serviceaccount:k8s-agent:k8s-agent-sa
  reason: "Confirming PSQL credentials for keep-backend after auth alert"
  command: "kubectl get secret keep-postgres-credentials -n keep -o jsonpath={.data.password}"
YAML
)
echo "request uid: $REQUEST_UID"
```

### 3. Poll the controller HTTP API

All state reads go through the API. Reachable cluster-internally without
oauth2-proxy (port 80 is fronted by the sidecar, but `/requests*` is on a
`--skip-auth-route`):

```bash
TOKEN=$(cat /var/run/secrets/sudo-service/token)
BASE=http://sudo-service.sudo-service.svc.cluster.local
# Or externally: BASE=https://sudo.andrewgarrett.dev — same routes.

while :; do
  STATE=$(curl -sS "$BASE/requests/$REQUEST_UID" -H "Authorization: Bearer $TOKEN")
  PHASE=$(jq -r .phase <<<"$STATE")
  echo "phase=$PHASE"
  case "$PHASE" in
    Executed|Failed|Denied|Expired) break ;;
  esac
  sleep 10
done
```

The Pushover push hits the admin within seconds; expect `Pending` for anywhere
from a few seconds to ~an hour. Auto-`Expired` after 1h with no approval.

For real-time updates, use the SSE stream instead of polling:

```bash
curl -sSN "$BASE/requests/$REQUEST_UID/events" -H "Authorization: Bearer $TOKEN"
# emits `data: {...}` lines per phase transition; closes on terminal phase
```

### 4. Fetch output (Executed only)

stdout+stderr live in a short-TTL Secret that the controller fronts on
`/output` — the requester SA never reads the Secret directly.

```bash
curl -sS "$BASE/requests/$REQUEST_UID/output" -H "Authorization: Bearer $TOKEN"
```

Output is GC'd `ttlSecondsAfterApproval` seconds after execution (default and
maximum 3600s). Large output is returned as a bounded prefix; the CLI reports
truncation and byte counts on stderr without changing command output on stdout.

### 5. Handle terminal states

- **`Executed`**: command ran and exited 0. Output capture/delivery is reported
  separately and may be truncated or unavailable; inspect `outputCaptureState`,
  `outputDeliveryState`, and `outputFailureReason` in status.
- **`Failed`**: command exited non-zero (see `.exitCode` and `/output`), or it
  failed before its exit code was known — e.g. the executor Job disappeared
  before the controller saw it complete. In the
  no-output case read `.failureReason` from the status response for the
  explanation. `/output` may or may not be populated.
- **`Denied`**: read `.denialReason` from the status response and report it
  verbatim. **Do NOT auto-retry** — address the reviewer's concern or ask
  the user how to proceed.
- **`Expired`**: nobody approved within an hour. Surface that.

## Richer jobs: mounts, other namespaces, big payloads

**Don't make a job to make a job.** If you find yourself putting `kubectl apply
-f - <<YAML ...` (a Job/Pod manifest) into `command`, express that pod *directly*
as the SudoRequest instead. The request can mount Secrets/PVCs, set env, run init
containers, and target another namespace — so the thing the human approves is the
actual pod that runs, and you avoid nesting heredocs and quoting inside `command`.

What you can set (a curated, reviewable subset — the human sees all of it on the
approve page):

- `namespace` — where the executor Job runs. **Default `sudo-service`.** To mount
  a Secret/PVC, the Job must run in **that resource's namespace** (pods can't
  mount cross-namespace). A Job in another namespace runs under that namespace's
  `default` ServiceAccount with **no cluster-admin** — which is exactly right for
  a "mount these files and run a script" task that doesn't call the API.
- `volumes` / `volumeMounts` — sources limited to `emptyDir`, `secret`,
  `configMap`, `persistentVolumeClaim`. `hostPath` and `projected` (which can
  carry a serviceAccountToken) are rejected.
- `env` / `envFrom` — e.g. a `secretRef` for credentials.
- `initContainers` — e.g. to stage a tool binary into a shared `emptyDir`. They
  run sequentially before the executor and inherit its locked-down
  securityContext and resource bounds. Only a reviewable subset of fields is
  permitted — `name`, `image`, **`command` (required)**, `args`, `env`,
  `envFrom`, `volumeMounts` — and anything else (`workingDir`,
  `imagePullPolicy`, securityContext, lifecycle hooks, volumeDevices,
  restartPolicy/sidecars, probes, ports, ...) is **rejected**, so the approve
  page can faithfully show what runs. An explicit `command` is required because
  the image's default entrypoint isn't shown to the reviewer.
- `stdin` — fed to the command's stdin. Use this instead of a heredoc to pipe a
  manifest to `kubectl apply -f -`; it travels as literal bytes, no shell quoting.
  Capped just under 1 MiB (it is carried in a Secret); oversized stdin is rejected
  at submission.
- `imagePullSecrets` — `[{name: ...}]` references to registry-credential Secrets
  (in the executor's namespace) the kubelet uses to pull a private `image` or
  init-container image. Unlike a mounted/env Secret they are **never exposed to
  the command** — the kubelet uses them only for registry auth — so they grant no
  extra capability; their names are still shown to the reviewer. The CLI exposes
  this as a repeatable `--image-pull-secret NAME`.
- `privileges.clusterAdmin` — defaults `true` in `sudo-service`, where it grants
  the cluster-admin executor SA. **Unavailable in other namespaces** (a
  cross-namespace Job can't be cluster-admin); setting both is rejected.

A request using any of these fields **always requires a human** — auto-approve
only applies to plain command+image one-liners.

Example request file — recover one file from a SeaweedFS volume (PVC + GCS-creds Secret both
live in `seaweedfs`, so the Job runs there under the default SA, no cluster-admin):

```yaml
reason: "Recover storypark image 419720067.jpg from volume 4787 after data-loss alert"
namespace: seaweedfs
image: chrislusf/seaweedfs:3.84
privileges: { clusterAdmin: false }    # implied off-namespace; explicit for clarity
command: |
  set -eu
  export RCLONE_CONFIG=/work/rclone.conf
  weed export -dir=/data -volumeId=4787 -o=/work/4787.tar -fileNameFormat='{{.Key}}'
  tar -xOf /work/4787.tar '4787,2957016f7719f2' > /work/recovered.jpg
  /tools/rclone rcat 'gcs:bucket/path/419720067.jpg' --size 735514 < /work/recovered.jpg
env:
  - { name: RCLONE_CONFIG, value: /work/rclone.conf }
initContainers:
  - name: copy-rclone
    image: rclone/rclone:latest
    command: ['/bin/sh','-c','cp $(command -v rclone) /tools/rclone && chmod 0555 /tools/rclone']
    volumeMounts: [{ name: tools, mountPath: /tools }]
volumeMounts:
  - { name: tools, mountPath: /tools, readOnly: true }
  - { name: work, mountPath: /work }
  - { name: data, mountPath: /data, readOnly: true }
  - { name: backup-config, mountPath: /etc/seaweedfs/gcs_creds.json, subPath: gcs_creds.json, readOnly: true }
volumes:
  - { name: tools, emptyDir: {} }
  - { name: work, emptyDir: {} }
  - { name: data, persistentVolumeClaim: { claimName: data-seaweedfs-volume-0, readOnly: true } }
  - { name: backup-config, secret: { secretName: backup } }
```

The executor container still runs as a non-root user with a read-only root
filesystem and all capabilities dropped. You get a writable `/tmp` and `$HOME`
(`/home/sudo-service`) for free — the controller mounts a bounded `emptyDir` at
each so the usual temp files and dotfile caches just work — but anything else on
the root filesystem is read-only, so write durable output to a mounted `emptyDir`
(e.g. `/work`), not to `/`. Mounting your own volume at `/tmp`, or setting `HOME`
yourself, opts out of the corresponding default.

## Gotchas

- `spec.requester` must match `request.userInfo.username` exactly — the
  apiserver VAP rejects mismatches. For an in-cluster SA, that's
  `system:serviceaccount:<namespace>:<sa-name>`.
- The default pod SA token has audience `https://kubernetes.default.svc.cluster.local`,
  not `sudo-service.andrewgarrett.dev`. If the controller returns 401, you
  almost certainly forgot to mount the audience-bound projected token from the
  Setup section.
- `ttlSecondsAfterApproval` defaults to 3600s and is hard-capped at 3600s by
  the CRD schema and the executor VAP. Don't ask for longer.
- Executor and requester init containers have modest CPU/memory scheduling
  requests but no sudo-service CPU, memory, or scratch-space limits. The
  executor has no runtime deadline. A separate 10-minute start deadline only
  fails a Pod that never reaches the executor (for example an unschedulable Pod
  or failed image pull); it does not stop a running command. Finished Jobs are
  retained for the requested post-approval TTL (with a short capture floor) so
  the controller can collect logs; that TTL begins after completion.
- In the `sudo-service` namespace the image runs under `cluster-admin` by
  default; a Job you target at another `namespace` runs under that namespace's
  unprivileged `default` ServiceAccount instead. Either way the human reviewer is
  the trust boundary — they see the image, namespace, privileges and mounts on the
  approve page. Don't expect server-side allowlisting of images.
- The widened pod fields are checked against a curated allowlist before the human
  sees them. The HTTP API rejects a bad spec with `400`; a CRD-created one is moved
  straight to `Denied` (`deniedBy=spec-validation`). Rejections include `hostPath`
  volumes, an init container that sets its own `securityContext`, and
  `privileges.clusterAdmin: true` combined with a non-default `namespace`.
- For a **non-cluster-admin** executor (any cross-namespace Job, or one with
  `privileges.clusterAdmin: false`), a referenced Secret may not be of type
  `kubernetes.io/service-account-token` — that would smuggle in API credentials —
  and every referenced Secret (volume, `env`, `envFrom`) **must already exist** in
  the target namespace when the Job is created, else the request fails. So create
  any credential Secret you mount *before* submitting the request.
- Commands are syntax-checked before they reach the human. The HTTP API rejects
  a syntactically-broken command with `400`; a CRD-created one is moved straight
  to `Denied` (`deniedBy=syntax-check`, parse error in `denialReason`) before any
  approval push is sent. The CLI also runs an optional portability check with
  the caller's host `sh -n`; that is not necessarily the selected profile's
  `/bin/sh`, so bypass a host-only dialect mismatch with `--no-validate`. Server
  profile-aware validation is authoritative and cannot be bypassed. Neither
  check makes the command safe or correct.

## Verification

Smoke test from inside the requester pod:

```bash
TOKEN=$(cat /var/run/secrets/sudo-service/token)
REQUEST_UID=$(kubectl create -f - -o jsonpath='{.metadata.uid}' <<'YAML'
apiVersion: sudo.andrewgarrett.dev/v1alpha1
kind: SudoRequest
metadata: { generateName: smoketest-, namespace: sudo-service }
spec:
  requester: system:serviceaccount:<your-ns>:<your-sa>
  reason: "verify sudo-service end-to-end"
  command: "kubectl get nodes"
YAML
)
# approve in the Pushover push, then:
curl -sS "http://sudo-service.sudo-service.svc.cluster.local/requests/$REQUEST_UID/output" \
  -H "Authorization: Bearer $TOKEN"
```
