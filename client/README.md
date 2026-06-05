# sudo-service client tooling

Requester-side tooling for [sudo-service](../README.md): the things an
in-cluster agent uses to *ask* for a human-approved privileged action. This is
the canonical home for both pieces; consumers (e.g. `werdnum/kube-config`)
install them from here.

| Path | What it is |
|---|---|
| `cli/sudo-service` | Self-contained Python 3 CLI (stdlib only). Creates a request via the controller HTTP API, waits for approval/execution, streams progress to stderr, and writes the command output to stdout. |
| `cli/tests/` | `unittest` suite for the CLI, exercised by `.github/workflows/client-test.yaml`. |
| `skills/sudo-service/SKILL.md` | Harness-agnostic agent skill describing when and how to use sudo-service. |

## CLI

The CLI has no third-party dependencies — it runs on any Python 3.11+:

```sh
client/cli/sudo-service \
  --reason "restart stuck pod" \
  -- kubectl delete pod foo -n bar
```

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

Because it's a single file with no dependencies, downstream images install it
**by reference** rather than vendoring a copy. For example, a consumer's
Dockerfile can fetch a pinned revision:

```dockerfile
# renovate: datasource=git-refs depName=werdnum/sudo-service
ARG SUDO_SERVICE_REF=main
RUN curl -fsSL \
      "https://raw.githubusercontent.com/werdnum/sudo-service/${SUDO_SERVICE_REF}/client/cli/sudo-service" \
      -o /usr/local/bin/sudo-service && chmod +x /usr/local/bin/sudo-service
```

## Installing the skill

`skills/sudo-service/SKILL.md` is plain Markdown with YAML frontmatter, so it
drops into any agent harness that reads skills/prompts from a directory (Claude
Code's `.claude/skills/`, etc.). Harnesses that can pull from git should
reference this path directly; otherwise vendor a copy and keep it in sync with
this file.
