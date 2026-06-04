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

### AI command summaries (optional)

When an OpenAI-compatible API key is configured, the controller generates a
concise, security-oriented **review aid** for each request as it enters the
`Pending` phase, caches it on the `SudoRequest` (`status.summary`), and shows it
on the approve page and in the requester status API. It is a review aid only —
explicitly **not** a substitute for the human reading the command — and the
whole feature is best-effort: a summarizer error never blocks an approval.

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
