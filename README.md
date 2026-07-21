# sudo-service

Human-approved privileged command execution for in-cluster agents.

`sudo-service` is a small Kubernetes controller that lets least-privilege
in-cluster agents (e.g. a read-only `k8s-agent`) request privileged actions —
read a Secret, restart a Pod, patch a Deployment, exec into a container — and
have them executed **only after a human approves the request out-of-band**
(via a Pushover push to the admin's phone). The agent gets to do useful work
without being handed cluster-admin; the human stays in the loop for anything
mutating, with a full audit trail.

Requests are modelled as a `SudoRequest` custom resource. On approval the
controller runs the command in an ephemeral executor Job bound to a
cluster-admin ServiceAccount, captures the output to a short-TTL Secret, and
returns it to the requester through an authenticated HTTP API. A pair of
`ValidatingAdmissionPolicies` provide defence-in-depth around who may create
executor Jobs and who may claim a given requester identity.

## Repository layout

| Path | Purpose |
|---|---|
| `*.go`, `templates/`, `Dockerfile` | Controller source + container image (built to `ghcr.io/werdnum/sudo-service`). |
| `charts/sudo-service/` | Helm chart deploying the controller, CRD, RBAC, admission policies, Service, ServiceMonitor, NetworkPolicy and Ingress. |
| `client/` | Requester-side tooling: the `sudo-service` CLI and the agent skill. Canonical home; consumers install these by reference. See [`client/README.md`](client/README.md). |
| `.github/workflows/build.yaml` | Builds + pushes the image and pins the new digest into the chart's `values.yaml`. |

## Deploying

The chart is consumed directly from this git repository — no chart repo
publishing step is required. With Argo CD, point an `Application` at
`charts/sudo-service` and supply the cluster-specific values (hostname, OIDC
issuer, secret names, ...). See the chart's
[`values.yaml`](charts/sudo-service/values.yaml) for the full set of knobs and
their defaults.

The Secrets the controller and its oauth2-proxy sidecar consume
(`sudo-service-pushover`, `sudo-service-oauth2-proxy`) are **not** part of the
chart — they are cluster-specific and expected to be provided out-of-band
(e.g. as SealedSecrets). The chart only references them by name.

### AI permission assessments (optional)

When an OpenAI-compatible API key is configured, the controller generates a
short, factual **permission request** for each request as it enters the
`Pending` phase. The versioned structured result is cached in
`status.permissionAssessment` and the same sentence appears on the approval
page and Pushover notification. It describes what pressing Approve permits; it
does not infer prior authorization, score risk, or recommend a decision. The raw
command and effective Pod spec remain ground truth. Generation is best-effort:
a model or validation error leaves the assessment absent and never blocks an
approval. `status.summary` remains readable for rolling upgrades and old records.

Enable it by setting `openai.enabled=true` and providing an API key Secret
(`openai.secretName`, default `sudo-service-openai`, key `api-key`). Point
`openai.baseURL` at any OpenAI-compatible endpoint to switch providers/models;
it defaults to the public OpenAI API with model `gpt-5.4-mini`.

The controller reads these env vars directly (the chart wires them from the
values above):

| Env | Default | Meaning |
|---|---|---|
| `OPENAI_API_KEY` | _(unset)_ | API key. **Unset disables the feature.** |
| `OPENAI_BASE_URL` | `https://api.openai.com/v1` | OpenAI-compatible base URL (no `/chat/completions` suffix). |
| `OPENAI_MODEL` | `gpt-5.4-mini` | Model id served by the base URL. |

### Command validation

The executor runs each request as `sh -c <command>`, so a command with a shell
syntax error (an unbalanced quote, a dangling pipe, an unterminated `$(`) can
never run. The controller parses every command's shell syntax — with a pure-Go
parser, never executing anything — and rejects broken ones up front rather than
spending the reviewer's attention on a doomed request:

- Requests submitted via the HTTP API are rejected at submission with `400 Bad
  Request`.
- Requests created directly against the CRD (which bypass the API) are caught by
  the controller and moved straight to `Denied` (with `deniedBy=syntax-check`
  and the parse error in `denialReason`) **before** any approval push is sent.

The parser uses the bash language variant — a superset of POSIX `sh` — for the
baseline syntax check, so it only rejects input that is broken in every shell.
Profile-aware preflight then uses the selected profile's machine-readable shell
and executable metadata to reject directly visible commands known to be absent
and to flag likely shell-dialect mistakes. It also warns about runtime package
installation, opaque base64 scripts, large heredocs, and likely long-running
commands. Warnings are conservative and advisory; static inspection cannot prove
runtime behavior. The human reviewer remains the trust boundary.

The CLI's early `sh -n` check uses the caller's host shell and is only an
optional portability hint; it may not be the implementation declared by the
selected profile. `--no-validate` skips that host check, but never skips the
server's syntax and profile-aware preflight.

### Executor profiles

Requests may select a friendly, server-owned `profile` instead of an arbitrary
`image`. The built-in `kubectl` (default) and `network-tools` profiles resolve to
digest-pinned images and publish their shell, executable, and capability metadata
at authenticated `GET /profiles`. The controller records `status.resolvedImage`
before approval; that exact digest is shown to the reviewer and is the one later
executed. Requesters cannot supply or override the resolution.

`profile` and `image` are mutually exclusive. Explicit raw images remain supported
for uncommon tools and are labeled as unprofiled; sudo-service does not pretend it
can infer their `/bin/sh` behavior or installed tools.

### Authorizing HTTP requesters

The requester HTTP API has two distinct Kubernetes checks. `TokenReview`
authenticates an audience-bound ServiceAccount token; a `SubjectAccessReview`
then requires that identity to have `create` on the virtual
`sudorequests/submit` subresource in the controller namespace. Possessing a
correctly scoped token alone is therefore not permission to create a request or
page an administrator.

Grant the HTTP permission to each requester ServiceAccount with ordinary RBAC:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: sudo-service-request-submitter
  namespace: sudo-service
rules:
  - apiGroups: ["sudo.andrewgarrett.dev"]
    resources: ["sudorequests/submit"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: k8s-agent-sudo-service-request-submitter
  namespace: sudo-service
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: sudo-service-request-submitter
subjects:
  - kind: ServiceAccount
    name: k8s-agent-sa
    namespace: k8s-agent
```

This permission is intentionally separate from direct CR authorization.
ServiceAccounts that submit `SudoRequest` objects through the Kubernetes API
still need `create` on `sudorequests`; that permission does not implicitly grant
HTTP submission, and `sudorequests/submit` does not permit direct CR creation.
NetworkPolicy controls reachability only and grants neither permission.

### Structured pod fields

Beyond a one-line `command`, a request can describe the executor pod directly —
`namespace`, `stdin`, `env`/`envFrom`, `volumes`/`volumeMounts`, `initContainers`,
`imagePullSecrets`, and a `privileges` block — so the human approves the actual privileged action
(which Secrets/PVCs it mounts, which namespace it runs in) rather than a `kubectl`
that creates an arbitrary pod. The fields reuse the upstream Kubernetes shapes but
are narrowed to a reviewable, non-escalating subset in `validateSpecExtras`
(no `hostPath`, no requester-set container `securityContext`, ...), with the same
two-path rejection as the syntax check (`400` on the HTTP path, `Denied` with
`deniedBy=spec-validation` on the CRD path). `cluster-admin` stays exclusive to
the controller namespace; cross-namespace Jobs run under the target namespace's
unprivileged `default` ServiceAccount. See
[`docs/widening-the-executor.md`](docs/widening-the-executor.md) for the full
design and the security rationale.

### Render locally

```sh
helm template sudo-service charts/sudo-service \
  --namespace sudo-service \
  -f my-values.yaml
```

## Required Secrets

| Secret | Keys | Used by |
|---|---|---|
| `sudo-service-pushover` | `token`, `user_key` | controller — Pushover approval pushes |
| `sudo-service-oauth2-proxy` | `client-secret`, `cookie-secret` | oauth2-proxy sidecar — OIDC login + session cookie |
| `sudo-service-openai` | `api-key` | controller — AI command summaries (only when `openai.enabled=true`) |
