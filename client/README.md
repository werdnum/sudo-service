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

Long-running work uses an explicit, bounded policy in the same request file:

```yaml
reason: "Run drift reconciliation after repairing its credential"
command: "ansible-playbook /workspace/ansible/site.yaml --check --diff"
image: "ghcr.io/example/ansible-runner@sha256:..."
execution:
  mode: managedJob
  resourceClass: long-running
  activeDeadlineSeconds: 3600
```

`managedJob` is sudo-service's controller-owned executor Job with curated
resources, UID correlation, bounded output, and foreground cleanup before a
terminal request phase. It does not authorize the command to create another
Job. Wait normally, or detach after submission and print the UID:

```sh
client/cli/sudo-service --request-file managed.yaml --detach --quiet
```

Detached submission does not bypass human approval. Query the authenticated
request API with the printed UID for lifecycle and output. See
[Managed Jobs](../docs/managed-jobs.md) for limits and failure semantics.

`--preview` writes the normalized effective request to stderr immediately before
submission. Request-building flags cannot be mixed with `--request-file`, so the
reviewed request has one unambiguous source. `--stdin-file` is the exception: it
may supply a separate literal payload when the request file does not itself set
`stdin`.

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
