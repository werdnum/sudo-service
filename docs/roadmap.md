# sudo-service product roadmap

## Purpose

This roadmap is grounded in a review of 58 `SudoRequest` records created from
2026-07-02 through 2026-07-20. It focuses on the points where the current
approval, execution, and requester interfaces repeatedly caused review fatigue,
expired requests, failed executions, or unnecessarily complicated commands.

The review found the following terminal-state distribution:

| Phase | Requests | Share |
| --- | ---: | ---: |
| Executed | 26 | 45% |
| Failed | 16 | 28% |
| Expired | 14 | 24% |
| Denied | 2 | 3% |

Twenty requests explicitly described themselves as a retry, replacement, or
follow-up to a prior attempt. These records are not a perfect measure of product
quality: some commands represented inherently exploratory infrastructure work,
and an approval does not prove that a review aid was useful. They do, however,
show stable workflow families and recurring friction that the product can
address.

## Habitual request families

The reviewed traffic was concentrated around a few operations:

1. Delete exact, investigated failed Jobs retained after a later successful run,
   especially `ansible-drift-metrics` and `cloudflare-tofu` Jobs.
2. Start one immediate Job from an existing CronJob, most commonly
   `ansible-drift-metrics`.
3. Read a Secret inside the executor and use it for a scoped API, SSH, or key
   derivation operation without returning the credential itself.
4. Run Ansible or router workflows that require Git, SSH, writable tool state,
   and substantially more time than a small one-shot `kubectl` command.
5. Create privileged diagnostic Pods for node filesystem inspection, then clean
   them up.
6. Restart a named workload or request an Argo CD hard refresh.
7. Read task output or non-secret metadata from internal services such as
   Semaphore.

The roadmap should make these common cases small and legible while preserving a
general escape hatch for genuinely novel administrative work.

## Design principles

- The human remains the approval authority. The service must not pretend to know
  whether a user previously authorized an action; that context is not reliably
  available to the current architecture.
- The review aid answers one question: **what permission is being requested if
  the reviewer presses Approve?**
- The raw command and effective Pod specification remain ground truth.
- Facts beat hypothetical warnings. Surface the target, scope, mutation, data
  access, destination, and privilege actually present; do not invent a generic
  reason to refuse every request.
- Repeated operations should become typed or templated requests, not ever-larger
  shell programs.
- Execution success, output delivery, and notification delivery are separate
  outcomes and must not be collapsed into one ambiguous phase.
- Prefer eliminating recurring requests at their source, while retaining an
  approval-gated path for legitimate one-off intervention.

## P0: rework the review aid into a permission request

### Problem

`summarizer.go` currently forces every model response into three prose fields:
"What you're approving", "Why you might refuse", and a risk/confidence rating.
That shape rewards the model for manufacturing a plausible objection even when
the request is routine and tightly scoped.

In the reviewed sample:

- 48 of 58 summaries were rated medium or high risk;
- 56 of 58 claimed high confidence;
- the average response was 101 words despite a target below 90;
- equivalent exact-name Job cleanups were inconsistently rated low and medium;
- only one response used the prompt's `routine, low-stakes` escape hatch.

The approval page then shows the full request, the long AI review card, and a
second generic cluster-admin warning. This repeats mechanism without directing
attention.

### Proposed interface

Generate structured output such as:

```json
{
  "request": "Start one immediate run of the ansible-drift-metrics CronJob.",
  "effects": ["CHANGES_CLUSTER", "CREATES_JOB"]
}
```

The sentence completes the implied phrase, "Hey, do you mind if I ...". It
should normally be under 30 words and describe the outcome rather than narrating
shell implementation details.

Material facts to preserve include:

- exact targets and target count;
- namespace, node, host, or external destination;
- read, create, change, restart, delete, or export effects;
- Secret or credential access;
- host-level or cluster-wide access;
- cleanup performed after the action.

The model must not infer prior authorization, recommend approval or denial,
invent hypothetical compromise scenarios, repeat generic cluster-admin risks,
or review correctness.

Render the result as **Permission requested**, with optional factual badges such
as `READ-ONLY`, `CHANGES CLUSTER`, `DELETES RESOURCE`, `READS SECRET`,
`EXTERNAL EGRESS`, `HOST ACCESS`, `SECURITY CONFIG`, and `BROAD SCOPE`.
Do not require a risk rating, confidence field, or warning paragraph.

Use the permission sentence in the Pushover notification as well as the approval
page. Keep detailed model output, if any, collapsed by default. Merge or demote
the generic privilege banner so the page does not warn about the same capability
multiple times.

### Implementation notes

- Add a versioned structured assessment to `SudoRequestStatus`; retain the
  existing `summary` field temporarily for old records and rolling upgrades.
- Use a strict response schema and validate effect labels server-side.
- Store the prompt/schema version and model identifier for auditability.
- Keep summary generation best-effort and non-blocking for approval.
- Build regression fixtures from the reviewed request corpus, with sensitive
  values removed.

### Acceptance criteria

- Routine permission text is one sentence and no more than 30 words in the
  normal case.
- Semantically equivalent exact-name cleanup commands produce the same factual
  effects.
- Credential use without an external credential sink is not described as
  exfiltration.
- Broad selectors, host access, credential output, and external destinations
  are named explicitly when present.
- The page remains fully usable if generation fails.

## P0: repair the approval notification lifecycle

### Problem

The approval link token expires after 15 minutes, while a Pending request does
not become Expired until one hour. The authenticated administrator queue already
supports tokenless approval, but an administrator who follows an expired
Pushover URL containing `t=` sees a token error instead of falling back to that
still-actionable queue path.

The controller performs model work before notification, which delays delivery
but is not itself a state race. The meaningful failure window is after Pushover
accepts a notification and before the controller persists the Pending phase and
token hash. A failed status update can cause reconciliation to mint a new token
and send another notification, leaving a duplicate notification whose earlier
link is dead.

### Proposed changes

- When an authenticated administrator follows an expired notification link for
  a still-Pending request, fall back to the normal tokenless approval view.
- Consider aligning notification-token lifetime with Pending lifetime to make
  unauthenticated link behavior less surprising.
- Persist an approvable state before emitting its notification.
- Track notification state separately (`Notifying`, `Notified`, or equivalent)
  so delivery retries do not remint unrelated state or produce dead predecessor
  links.
- Generate the permission sentence independently of notification delivery and
  do not regenerate it on every Pushover retry.
- Consider a reminder near request expiry, without automatically approving or
  resubmitting anything.
- Before making authenticated fallback the primary path, verify explicit CSRF
  protection and SameSite behavior for approval and denial POSTs.

### Acceptance criteria

- An expired Pushover token does not block an authenticated administrator from
  opening and acting on a still-Pending request.
- A delivered notification always references a persisted approvable state.
- Status-update or notification retries do not produce duplicate notifications
  with stale links.
- Approval and denial POSTs have tested CSRF and cookie-policy protection.

## P0: bound output and preserve the real execution result

### Problem

Executor output is materialized into one Kubernetes Secret. A successful audit
command that listed historical requests produced more than 1 MiB, so Secret
creation failed and the request was marked Failed even though the command exited
zero.

The current phase therefore conflates command execution with output capture.

### Proposed changes

- Bound captured output before Secret creation and mark it as truncated.
- Record command exit code independently from output capture/delivery state.
- Add metadata such as total bytes observed, retained bytes, truncation flag,
  and a digest where useful.
- Return a clear result such as `command succeeded; output truncated`, rather
  than changing command success into request failure.
- Prefer a paginated history/export API for large administrative datasets.

### Acceptance criteria

- Executor output can never cause a Secret-size validation failure.
- Exit-zero commands remain recorded as successfully executed even when output
  is truncated or delivery fails.
- The requester CLI communicates truncation without corrupting stdout.

## P1: bring the CLI to parity with the request schema

### Problem

The API and CRD support environment variables, `envFrom`, volumes, mounts, and
init containers. The bundled CLI exposes namespace, stdin, cluster-admin opt-out,
and image pull Secrets, but not the rest of the structured request.

Agents consequently continue to put large heredocs, encoded scripts, and
`kubectl apply` payloads in `command`. Fifteen reviewed commands exceeded 1,000
characters even though the service now has a richer request representation.

### Proposed changes

- Add `--request-file` for a complete YAML or JSON request body.
- Optionally add ergonomic flags for the most common structured fields.
- Print a normalized local preview before submission when requested.
- Ensure the CLI and HTTP API share validation and error terminology.
- Update the bundled skill to prefer structured requests and `--stdin-file`
  whenever a command would contain a manifest, heredoc, or encoded payload.

### Acceptance criteria

- Every reviewable request shape supported by the HTTP API can be submitted by
  the CLI without direct CRD construction.
- The common rich-job examples do not require nested shell/YAML quoting.

## P1: add curated executor profiles and meaningful preflight

### Problem

Repeated requests failed because the chosen image lacked `ssh`, `ssh-keygen`, or
`cf-terraforming`; attempts to install packages failed in the non-root executor;
and commands used `pipefail` despite execution through `/bin/sh`. Other retries
addressed HOME, SSH control-path, Git credential, or heredoc expansion behavior.

The CLI preflight runs the host's `sh -n`, while server validation parses with
`mvdan.cc/sh` in Bash-dialect mode. Neither proves compatibility with the
executor's `/bin/sh`, and the dialect mismatch explains why constructs such as
`set -o pipefail` can pass submission validation and fail at runtime.

### Proposed changes

- Publish server-resolved, digest-pinned profiles such as `kubectl`,
  `network-tools`, `ansible`, and `cloudflare`.
- Give each profile a machine-readable executable/capability manifest.
- Add submission-time warnings or failures for:
  - Bash-only constructs under `/bin/sh`, especially `pipefail`;
  - required executables absent from a known profile;
  - runtime package installation in the locked-down executor;
  - opaque base64-wrapped scripts;
  - large heredocs better represented as stdin;
  - commands likely to exceed the one-shot executor profile, as an advisory
    heuristic rather than a correctness claim.
- Prefer a profile alias in review surfaces while continuing to show the exact
  resolved image digest.

### Acceptance criteria

- For commands whose required executable is directly visible in argv, a known
  profile warns or fails before approval when its capability manifest says that
  executable is absent.
- The standard documented examples run with the selected profile's shell and
  filesystem constraints.

## P1: add typed helpers for habitual operations

### Problem

Small operational actions are repeatedly reconstructed as arbitrary shell.
That increases quoting failures, makes review inconsistent, and prevents safe
structural validation.

### Initial helpers

```text
sudo-service job delete <namespace>/<name>...
sudo-service cronjob run <namespace>/<name>
sudo-service workload restart <namespace>/<kind>/<name>
sudo-service secret read <namespace>/<name> <key>
```

These helpers should produce exact, legible commands or typed request actions,
reject unintended broad selectors, and generate the permission sentence without
guessing at arbitrary shell semantics.

The existing auto-approve implementation matches exact commands or token
prefixes and is not currently wired through the chart. Do not enable broad
prefix rules for operations such as `kubectl delete`; a matching prefix can hide
an unsafe selector later in argv. Typed operations are a safer basis for any
future selective auto-approval.

### Acceptance criteria

- Exact Job cleanup and CronJob triggering no longer require hand-built shell.
- Broad operations require an explicitly broad typed request, not an accidental
  suffix accepted by a prefix rule.
- Generated resource names and cleanup behavior are deterministic and visible.

## P1: support long-running work and privileged diagnostics directly

### Problem

Executor and init containers receive a fixed 500m CPU and 256 MiB memory limit.
One long Ansible sequence exited 137 and ultimately succeeded only after the
request created a separate Kubernetes Job. Node diagnostics likewise created
privileged or host-mounted child Pods because the direct executor schema rejects
root, host PID, and hostPath.

This creates two review gaps:

1. the displayed "ground-truth" executor Pod is not the child workload that does
   the consequential work;
2. the child workload's lifecycle and output are not intrinsically tied to the
   SudoRequest.

### Proposed changes

- Add bounded execution profiles for resource and duration needs rather than
  arbitrary requester-controlled resource overrides.
- Add a first-class asynchronous Job mode whose Job UID, lifecycle, timeout,
  result, and cleanup remain associated with the SudoRequest.
- Detect and flag commands that create child Pods or Jobs.
- Design a constrained node-diagnostic action, or add explicit reviewed
  capabilities for node selection, read-only host filesystem access, root, and
  host PID. These capabilities must be admission-enforced and rendered on the
  approval page.
- Avoid presenting the executor Pod as complete ground truth when the approved
  command creates a more privileged child workload.

### Acceptance criteria

- Long Ansible/drift work does not depend on a 256 MiB foreground shell.
- A node diagnostic's host access and cleanup are explicit in the request model.
- No approved child workload can outlive the request without a recorded,
  review-visible lifecycle policy.

## P1: improve expiry, retries, and duplicate handling

### Problem

Fourteen reviewed requests expired. Several cleanup actions were submitted three
or four times before one was approved, and retry context currently lives only in
free-form reason text.

### Proposed changes

- Add `retryOfUID` or `supersedesUID` lineage.
- Let the original requester clone or resubmit an Expired request through the
  requester CLI/API.
- If administrators receive a resubmit action, record the administrator as the
  resubmitter instead of attributing the new request to the original requester;
  otherwise keep the administrator action to copying or proposing a request.
- Warn when an equivalent request is already Pending.
- Render superseded requests distinctly in history.
- Allow the CLI to resubmit a known request without reconstructing its fields.
- Never automatically retry a human denial.

### Acceptance criteria

- A reviewer can identify the latest request in a retry chain immediately.
- Equivalent Pending actions are not accidentally queued multiple times.
- Resubmission preserves the exact prior payload, records its lineage, and does
  not misrepresent who submitted the new request.

## P1: authorize request creation and resist notification spam

### Problem

The HTTP API verifies a Kubernetes TokenReview token with the expected audience,
then permits the authenticated ServiceAccount to create a request. It does not
apply a requester allowlist or a Kubernetes authorization check for this action.
The NetworkPolicy admits traffic to the HTTP port from Pods in any namespace.
Consequently, request creation and administrator phone notifications have a
broader trust boundary than direct `SudoRequest` RBAC implies.

### Proposed changes

- Define an explicit requester authorization policy, preferably using a
  dedicated Kubernetes RBAC permission checked with SubjectAccessReview or an
  equivalently auditable allowlist.
- Rate-limit request creation and notification delivery per requester while
  preserving actionable error messages.
- Record authorization denials and throttling as metrics without putting bearer
  tokens or request commands in logs.
- Document the relationship between HTTP authorization, direct CRD RBAC, and
  NetworkPolicy rather than implying that they enforce the same boundary.

### Acceptance criteria

- Possession of an audience-bound ServiceAccount token alone is insufficient to
  page an administrator or create a `SudoRequest`.
- An authorized requester's normal retry workflow is not mistaken for abuse.
- A single requester cannot create unbounded approval notifications.

## P2: provide real history and audit interfaces

### Problem

The administrator index shows only the 20 most recent terminal requests, and
terminal rows do not link to a detail view. The requester API can fetch only an
already-known UID and cannot list even the requester's own history.

This forced the roadmap audit through a privileged full-CR listing, which then
overflowed output capture.

### Proposed changes

- Add terminal request detail pages.
- Add filters for time, requester, phase, image/profile, namespace, and action.
- Add pagination and bounded JSON/CSV export.
- Add a requester endpoint that lists only records owned by the authenticated
  requester.
- Add a separately authorized administrator audit endpoint with an explicit
  redaction contract.
- Keep raw command/spec visibility separate from default summary exports where
  operational history may contain sensitive values.
- Define a terminal-request retention policy. Use bounded garbage collection
  only after required audit metadata is archived or exported; do not allow
  terminal CRs and their status payloads to grow in etcd indefinitely.

### Acceptance criteria

- A month of request metadata can be inspected without cluster-admin or a
  greater-than-1-MiB response.
- Every terminal row can be opened and correlated with retries, execution result,
  and output-retention state.
- Retention duration, archive destination, and deletion behavior are explicit
  and testable.

## P2: eliminate recurring requests at the source

Some frequent requests should disappear rather than become easier to repeat.

Cross-repository GitOps follow-ups include:

- reduce `ansible-drift-metrics` failed Job history and its current seven-day
  `ttlSecondsAfterFinished` after confirming that logs and metrics are durably
  captured;
- ensure `cloudflare-tofu` Jobs receive an appropriate TTL at their creation
  point;
- revisit alerts that remain active solely because an investigated failed Job
  object is retained after later successful runs;
- expose a narrow, approval-gated trigger for routine "run drift now" requests
  rather than requiring a general cluster-admin shell.

These changes belong in `kube-config`, but their motivation and dependency stay
recorded here because they directly shape sudo-service traffic.

## P2: add operational metrics and evaluate the interface

The service exposes a Prometheus handler but currently has no product-level
request metrics. Add counters and histograms for:

- requests by terminal phase and action/profile;
- creation-to-approval latency;
- approval-to-start latency and execution duration;
- token-expired and request-expired outcomes;
- notification retries;
- requester authorization denials and rate limiting;
- output capture failures and truncations;
- executor profile/image usage;
- retries, superseded requests, and duplicate warnings;
- permission-generation latency and failures.

Build a sanitized evaluation corpus from real requests. Include paired cases
where one material property changes: exact name versus `--all`, local Secret use
versus external credential transfer, one restart versus namespace-wide restart,
read-only host inspection versus mutation, and a known image versus an arbitrary
image.

Suggested initial quality targets:

- normal visible permission text at or below 30 words;
- equivalent operations receive equivalent effect labels;
- no invented external destination or data-loss claim;
- every broad selector, credential output, host access, and external sink is
  represented explicitly;
- the generated permission sentence is absent rather than misleading when the
  model or schema validation fails.

## P3: consistency and existing security debt

- Reconcile the output-retention documentation: the bundled skill describes a
  600-second default while the controller and CRD currently default to 3,600
  seconds.
- Triage open issue #4, where the executor ValidatingAdmissionPolicy gates Jobs
  but not direct Pods using the executor ServiceAccount.
- Recheck open issues #2 and #3 against current controller/chart behavior and
  close or update them where later work has superseded the report.
- Version future review/request interfaces rather than overloading the existing
  free-form `summary` field indefinitely.

## Delivery sequence

The roadmap should be delivered in independently reviewable slices:

1. Bounded output plus separate execution and output-delivery status.
2. Expired-link administrator fallback and notification state sequencing.
3. Structured permission-request generator and compact UI/Pushover rendering.
4. Explicit requester authorization and notification rate limiting.
5. CLI `--request-file`, structured preview, executor profiles, and preflight.
6. Typed exact-Job cleanup and CronJob-run helpers.
7. Request history/detail/list/export APIs, retention, and retry lineage.
8. Long-running Job and node-diagnostic execution modes.
9. Cross-repository Job-retention fixes, metrics, and ongoing evaluation.

Security-boundary changes, especially requester authorization, administrator
approval paths, executor Pod admission, typed auto-approval, host access, and
new privilege toggles, should remain separate from usability-only changes so
each can receive focused threat-model review.
