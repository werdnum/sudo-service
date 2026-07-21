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

For recurring administrative operations, prefer the typed helpers. They send
exact resource fields rather than requester-built shell. The controller validates
the action, compiles the command, and derives a separate permission request from
the same fields; the requester-supplied `--reason` remains the incident/task
context and cannot override that permission text.

```sh
sudo-service --reason "remove retained failures after a healthy rerun" \
  job delete concourse/old-build monitoring/old-check

sudo-service --reason "rerun drift after repairing credentials" \
  cronjob run ansible/ansible-drift

sudo-service --reason "reload the repaired configuration" \
  workload restart ml-bot/deployment/family-assistant

sudo-service --reason "diagnose the database authentication failure" \
  secret read keep/keep-postgres-credentials password
```

Job deletion takes only exact `<namespace>/<name>` operands. Workload restart is
limited to Deployment, StatefulSet, and DaemonSet. Secret read requires one exact
Secret and key. Selector/options are rejected rather than appended to a trusted
prefix. CronJob run creates a visible `<cronjob>-manual-<UTC timestamp>` Job and
sets `ttlSecondsAfterFinished: 86400` in its manifest before creating it.

Typed requests still require human approval. The create response prints the
server-derived permission request and canonical command; `--preview` shows the
semantic action submitted to the server.

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
