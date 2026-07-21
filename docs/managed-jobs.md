# Managed Jobs

`managedJob` is sudo-service's explicit mode for commands which need more time or
memory than the standard executor. It is not permission for the approved shell to
create another Kubernetes Job or Pod. The managed workload is the same
controller-created executor Job already associated with every SudoRequest, with a
larger curated resource class and a stricter, durable lifecycle.

## Request shape

Use a complete request file:

```yaml
reason: Re-run the drift playbook after repairing its SSH credential
command: ansible-playbook /workspace/ansible/site.yaml --check --diff
image: ghcr.io/example/ansible-runner@sha256:...
execution:
  mode: managedJob
  resourceClass: long-running
  activeDeadlineSeconds: 3600
```

The initial `long-running` class is controller-owned:

| | Requests | Limits |
|---|---|---|
| CPU | 250m | 2 cores |
| Memory | 256 MiB | 2 GiB |
| Ephemeral storage | 256 MiB | 2 GiB |

The deadline is required and must be 300–7200 seconds. Callers cannot provide
arbitrary resource quantities. An empty execution policy remains the standard
foreground class (50m/64 MiB requested, 500m/256 MiB limited) with a controller-
stamped 3600-second hard deadline.

Submit and wait normally, or detach after submission:

```sh
sudo-service --request-file request.yaml
sudo-service --request-file request.yaml --detach --quiet
```

Detached submission prints the SudoRequest UID. It does not bypass approval; it
only stops the local CLI poll. The authenticated requester API remains the source
for status and bounded output.

## Review contract

Before approval, the page and Pushover notification show:

- the effective execution mode and curated resource class;
- CPU and memory requests/limits;
- the exact hard deadline;
- the foreground-cleanup-before-terminal policy;
- the full Job template, including the Pod, rather than presenting only a
  partial Pod description.

The raw command, image, mounts, environment, stdin, and privilege state remain
visible. A managedJob request is never eligible for command-prefix auto-approval.

## Durable lifecycle

The controller records the random Job name and Kubernetes UID before treating it
as the approved workload. Managed lifecycle state is persisted on the
SudoRequest:

```text
Created -> Running -> ResultCaptured -> CleanupRequested -> Cleaned
```

The transition order is deliberate:

1. The Job UID is recorded, preventing replay or adoption of a replacement.
2. The exact command exit code and bounded output metadata/Secret are persisted.
3. Cleanup becomes `CleanupRequested` without discarding that result.
4. The controller issues foreground deletion with a UID precondition.
5. Only after an uncached read observes the Job gone does the request become
   terminal (`Executed` or `Failed`) and lifecycle become `Cleaned`.

A controller restart at any persisted state resumes that transition rather than
creating another Job. A stuck deletion leaves the request Approved with captured
output intact and visible. If the controller is unavailable while a command is
running, Kubernetes `activeDeadlineSeconds` remains the independent stop bound.
Deleting the SudoRequest is also finalizer-gated on foreground deletion of the
UID-bound managed Job.

Output uses the same bounded capture contract as standard execution: the full
observed byte count and SHA-256 digest are recorded, only the retained prefix is
stored, and output delivery failure does not rewrite a known command exit code.

## Threat model and excluded diagnostics

This mode addresses resource exhaustion, runaway duration, replay, replacement,
output loss, and orphaned controller-owned Jobs. It does not widen the Pod's
security context: non-root, read-only root filesystem, dropped capabilities,
restricted volume sources, and existing ServiceAccount rules still apply.

Privileged node diagnostics are intentionally not part of this change. Root,
host PID, host networking, node selection, and hostPath each change the trust
boundary and require a separate request schema, approval rendering, and admission
policy. They must not be smuggled into `managedJob` as arbitrary Pod fields.

Commands which themselves create Pods or Jobs remain arbitrary cluster-admin
shell and do not gain managed lifecycle guarantees for those children. Detection
and review warnings belong with the profile-aware preflight work; the safe path
for long work is to run the real command directly in this UID-correlated Job.
