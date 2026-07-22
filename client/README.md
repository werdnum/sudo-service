# sudo-service client tooling

Requester-side tooling for [sudo-service](../README.md): the things an
in-cluster agent uses to *ask* for a human-approved privileged action. This is
the canonical home for both pieces; consumers (e.g. `werdnum/kube-config`)
install them from here.

| Path | What it is |
|---|---|
| `cli/sudo-service` | Python 3 CLI. Creates a request via the controller HTTP API, waits for approval/execution, streams progress to stderr, and writes the command output to stdout. |
| `cli/requirements.txt` | Pinned runtime dependency used to safely parse complete YAML request files. |
| `cli/tests/` | `unittest` suite for the CLI, exercised by `.github/workflows/client-test.yaml`. |
| `skills/sudo-service/SKILL.md` | Harness-agnostic agent skill describing when and how to use sudo-service. |

## CLI

The CLI runs on Python 3.11+ and uses PyYAML's safe loader for request files:

```sh
python3 -m pip install --requirement client/cli/requirements.txt
```

Then run it directly:

```sh
client/cli/sudo-service \
  --reason "restart stuck pod" \
  -- kubectl delete pod foo -n bar
```

The default `kubectl` executor is a server-resolved, digest-pinned profile. Select
another curated profile explicitly when appropriate:

```sh
client/cli/sudo-service \
  --reason "Diagnose DNS resolution from the cluster" \
  --profile network-tools \
  -- dig example.com
```

The create response prints the friendly profile and exact resolved digest. Use
`--image` instead for an arbitrary image; `--profile` and `--image` are mutually
exclusive. Static preflight warnings are printed even with `--quiet` because they
describe review-relevant footguns, not progress chatter.

When a task needs an uncommon combination of tools, the CLI can construct an
ordinary [Nixery](https://nixery.dev/) image. Repeat `--tool`; the client sorts
and deduplicates the top-level Nix package attributes and submits the generated
URL through the existing raw `image` field:

```sh
client/cli/sudo-service \
  --reason "Run infrastructure diagnostics over SSH" \
  --tool openssh --tool ansible --tool kubectl --tool opentofu \
  -- ssh host.example true
```

This produces
`nixery.dev/arm64/shell/ansible/kubectl/openssh/opentofu`, which is shown in full
on the existing approval page. `--tool` is only an arm64 client convenience; it
adds no profile, CRD, controller, status, or approval semantics. Use `--image`
directly for another architecture, registry, or hand-written Nixery path. New
Nixery combinations can have a substantial cold-pull cost.

Nixery images do not necessarily contain a passwd entry for sudo-service's
numeric UID. Most tools do not care, but OpenSSH exits with
`No user exists for uid 1000`. If SSH is needed, use a structured request file
to mount a small passwd file prepared by a non-root init container:

```yaml
reason: Run infrastructure diagnostics over SSH
image: nixery.dev/arm64/shell/ansible/kubectl/openssh/opentofu
command: ssh host.example true
initContainers:
  - name: write-passwd
    image: nixery.dev/arm64/shell
    command: [/bin/sh, -c]
    args:
      - "printf '%s\\n' 'sudo-service:x:1000:1000:sudo-service:/home/sudo-service:/bin/sh' > /identity/passwd"
    volumeMounts: [{name: identity, mountPath: /identity}]
volumeMounts:
  - {name: identity, mountPath: /etc/passwd, subPath: passwd, readOnly: true}
volumes:
  - {name: identity, emptyDir: {sizeLimit: 1Mi}}
```

This workaround uses only the existing reviewed pod fields and preserves the
non-root user, read-only root filesystem, and dropped capabilities.

By default the CLI waits up to 12 hours for approval and completion. This is
only a local client deadline: it does not stop the server-side executor. Use
`--timeout 0` to wait indefinitely, or `--detach` to print the submitted request
UID on stdout and return immediately without polling. Detach works for any
request; retain the UID to query its status later.

For requests that need environment variables, mounts, volumes, init containers,
or other structured fields, put the complete HTTP request body in YAML or JSON:

```yaml
# request.yaml
reason: "Inspect one application database with its existing credentials"
command: "psql -c 'select version()'"
image: postgres:17
namespace: example
envFrom:
  - secretRef: {name: database-credentials}
privileges: {clusterAdmin: false}
```

```sh
client/cli/sudo-service --request-file request.yaml --preview
```

`--preview` writes the normalized effective request to stderr immediately before
submission. Request-building flags cannot be mixed with `--request-file`, so the
reviewed request has one unambiguous source. `--stdin-file` is the exception: it
may supply a separate literal payload when the request file does not itself set
`stdin`.

To explicitly resubmit an `Expired` or `Failed` request without rebuilding its
payload, use its UID:

```sh
client/cli/sudo-service --retry "$REQUEST_UID" --preview
```

The successor requires a fresh approval and records immutable lineage. Retry cannot
be combined with payload-building flags. The service may return an existing UID when
the same requester already has an equivalent active request; the CLI reports that
deduplication even under `--quiet`. It never reveals another requester's match and
never automatically retries a denial.

```sh
client/cli/sudo-service \
  --request-file request.yaml \
  --stdin-file manifest.yaml
```

Prefer that form to embedding a heredoc, encoded script, or manifest in
`command`. Both JSON and complete safe-loaded YAML are supported. YAML parsing
does not construct application-specific Python objects.

Configuration (flags override env, which override in-cluster defaults):

| Flag | Env | Default |
|---|---|---|
| `--url` | `SUDO_SERVICE_URL`, `K8S_AGENT_SUDO_SERVICE_URL` | `http://sudo-service.sudo-service.svc.cluster.local` |
| `--token-file` | `SUDO_SERVICE_TOKEN_FILE` | `/var/run/secrets/sudo-service/token` |
| `--timeout` | — | `43200` seconds (12 hours; `0` means unlimited) |

Run the tests:

```sh
cd client/cli && python3 -m unittest discover -s tests -v
```

## Installing the CLI elsewhere

Downstream images install the CLI **by reference** rather than vendoring a copy.
Install its pinned dependency and fetch the script from the same revision:

```dockerfile
# renovate: datasource=git-refs depName=werdnum/sudo-service
ARG SUDO_SERVICE_REF=main
RUN python3 -m pip install --no-cache-dir PyYAML==6.0.3 && \
    curl -fsSL \
      "https://raw.githubusercontent.com/werdnum/sudo-service/${SUDO_SERVICE_REF}/client/cli/sudo-service" \
      -o /usr/local/bin/sudo-service && chmod +x /usr/local/bin/sudo-service
```

## Installing the skill

`skills/sudo-service/SKILL.md` is plain Markdown with YAML frontmatter, so it
drops into any agent harness that reads skills/prompts from a directory (Claude
Code's `.claude/skills/`, etc.). Harnesses that can pull from git should
reference this path directly; otherwise vendor a copy and keep it in sync with
this file.
