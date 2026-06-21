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

## When to use this skill

- An alert points at a private Secret you need to read.
- You want to `kubectl rollout restart` / `patch` / `delete` something to
  remediate.
- You want to `kubectl exec` into a Pod for a one-off diagnostic.
- Any time you'd ordinarily ask the human "can you run X for me?" in chat.

Don't use it for read-only operations you can already do (`kubectl get/list/watch`).

## Setup the requester pod needs (one-time)

**RBAC.** The requester ServiceAccount needs `create` on
`sudorequests.sudo.andrewgarrett.dev`. The `k8s-agent` ClusterRole already
grants this — anything else needs a similar rule. `get`/`list`/`watch` is
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

It creates the request through the controller HTTP API, polls
`/requests/{uid}`, prints progress on stderr, and writes the command output
from `/requests/{uid}/output` to stdout when the command executes. For shell
pipelines or JSONPath braces, pass a single command string:

```bash
sudo-service \
  --reason "Confirming PSQL credentials for keep-backend after auth alert" \
  --command 'kubectl get secret keep-postgres-credentials -n keep -o jsonpath={.data.password}'
```

### 1. Draft the request

Pick the smallest, most legible command. The human sees the verbatim
`command` + `reason` + `image` on the approve page, so be surgical — e.g.
`kubectl get secret foo -n bar -o jsonpath={.data.x}`, not a kitchen-sink
shell pipeline.

```yaml
apiVersion: sudo.andrewgarrett.dev/v1alpha1
kind: SudoRequest
metadata:
  generateName: agent-              # arbitrary prefix
  namespace: sudo-service            # always this namespace
spec:
  requester: system:serviceaccount:<your-ns>:<your-sa>  # MUST match your SA
  reason: "One sentence: WHY you need this, what alert/task it's for."
  command: "exact shell command, single string"
  # image: alpine/k8s:1.35.5          # optional, this is the default
  # ttlSecondsAfterApproval: 600       # output retention, default 600s, max 3600
```

A `ValidatingAdmissionPolicy` enforces `spec.requester == request.userInfo.username`
on direct CRD writes, so don't put another SA in there.

### 2. Submit and capture the request uid

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

Output is GC'd `ttlSecondsAfterApproval` seconds after execution (default 600s).

### 5. Handle terminal states

- **`Executed`**: command ran, exit 0, output available on `/output`. Report.
- **`Failed`**: command ran but exited non-zero, OR the executor Job
  disappeared before the controller saw it complete (look at the `Event` on
  the SudoRequest for the reason). `/output` may or may not be populated.
- **`Denied`**: read `.denialReason` from the status response and report it
  verbatim. **Do NOT auto-retry** — address the reviewer's concern or ask
  the user how to proceed.
- **`Expired`**: nobody approved within an hour. Surface that.

## Gotchas

- `spec.requester` must match `request.userInfo.username` exactly — the
  apiserver VAP rejects mismatches. For an in-cluster SA, that's
  `system:serviceaccount:<namespace>:<sa-name>`.
- The default pod SA token has audience `https://kubernetes.default.svc.cluster.local`,
  not `sudo-service.andrewgarrett.dev`. If the controller returns 401, you
  almost certainly forgot to mount the audience-bound projected token from the
  Setup section.
- `ttlSecondsAfterApproval` defaults to 600s and is hard-capped at 3600s by
  the CRD schema and the executor VAP. Don't ask for longer.
- The image runs under `cluster-admin`. If you specify a non-default `image`,
  the human reviewer is the trust boundary — they see the image on the
  approve page next to the command. Don't expect server-side allowlisting.
- Commands are syntax-checked before they reach the human. The HTTP API rejects
  a syntactically-broken command with `400`; a CRD-created one is moved straight
  to `Denied` (`deniedBy=syntax-check`, parse error in `denialReason`) before any
  approval push is sent. The CLI runs the same check locally with `sh -n` before
  submitting — bypass it with `--no-validate`. This only catches shell syntax
  errors (unbalanced quotes, dangling pipes); it does **not** validate that the
  command does anything sensible.

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
