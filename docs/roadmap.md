# sudo-service product roadmap

## Purpose and evidence

This roadmap is grounded in 58 `SudoRequest` records created from 2026-07-02
through 2026-07-20. It records product decisions as well as proposed work so a
rejected design does not quietly return as an implementation task.

The original sample contained:

| Phase | Requests | Share |
| --- | ---: | ---: |
| Executed | 26 | 45% |
| Failed | 16 | 28% |
| Expired | 14 | 24% |
| Denied | 2 | 3% |

Twenty requests described themselves as a retry, replacement, or follow-up.
The repeated friction was review fatigue, expired or duplicate requests,
oversized output, shell quoting, missing executables, and foreground clients
giving up on legitimate long-running work.

The sample did **not** demonstrate a general need for bespoke wrappers around
ordinary `kubectl` operations, requester spam controls, a product metrics
taxonomy, or privileged node-diagnostic modes.

## Product principles

- The human remains the approval authority. The service does not infer prior
  authorization that the architecture cannot reliably know.
- The review aid answers one question: **what permission is being requested if
  the reviewer presses Approve?**
- The raw command, image, and effective Pod specification remain ground truth.
- Facts beat generic warnings: show the target, scope, mutation, data access,
  destination, and privilege actually present.
- Prefer ordinary, composable request fields and thin client conveniences over
  permanent server-side semantic special cases.
- Remove recurring requests at their source before making them easier to repeat.
- Guardrails should address demonstrated privilege or reliability boundaries.
  Small CPU, memory, or time ceilings are not useful security boundaries for an
  explicitly approved cluster-administrator command.
- Execution, output capture, output delivery, and notification delivery are
  distinct outcomes.

## Shipped foundation

The following changes are merged into `main`:

1. [#36](https://github.com/werdnum/sudo-service/pull/36) adds complete YAML/JSON
   request files, normalized preview, and separate stdin files. Rich requests no
   longer require base64 payloads, nested manifests, or fragile heredocs.
2. [#37](https://github.com/werdnum/sudo-service/pull/37) bounds retained output
   and records command exit, capture, delivery, byte counts, truncation, and a
   digest independently. Successful commands no longer become failed requests
   because a Kubernetes Secret exceeded 1 MiB.
3. [#38](https://github.com/werdnum/sudo-service/pull/38) requires an explicit
   Kubernetes SubjectAccessReview permission for HTTP submission. The matching
   deployment RBAC landed in
   [kube-config #1296](https://github.com/werdnum/kube-config/pull/1296).
4. [#39](https://github.com/werdnum/sudo-service/pull/39) prevents direct Pods
   from borrowing the privileged executor ServiceAccount outside the
   controller-owned Job path.
5. [#40](https://github.com/werdnum/sudo-service/pull/40) persists approvable
   state before notification, reuses durable links across notification retries,
   aligns link and request expiry, provides authenticated fallback, and protects
   approval and denial POSTs against CSRF.
6. [#41](https://github.com/werdnum/sudo-service/pull/41) provides known-good
   executor profiles and conservative shell/executable preflight. Exact digests
   are an implementation property of curated defaults, not information the
   human reviewer is expected to audit.
7. [#42](https://github.com/werdnum/sudo-service/pull/42) replaces risk,
   confidence, and refusal prose with one factual permission sentence and a
   closed set of effect badges. It also ships sanitized regression fixtures.
8. [kube-config #1297](https://github.com/werdnum/kube-config/pull/1297) reduces
   stale drift Job retention, alerts from the latest durable result rather than
   old failed objects, and documents the existing narrow paths for immediate
   drift runs.

## Immediate delivery

### Thin Nixery client support

[PR #45](https://github.com/werdnum/sudo-service/pull/45) adds repeated `--tool`
arguments to the client. The client validates, sorts, and deduplicates top-level
Nix attributes and submits an ordinary raw image such as:

```text
nixery.dev/arm64/shell/ansible/kubectl/openssh/opentofu
```

This deliberately adds no CRD, controller, profile, status, API, approval-page,
or notification semantics. The full image URL is sufficient ground truth.
Uncommon OpenSSH numeric-UID setup is documented with the existing structured
init-container and volume fields rather than hidden behind a new server feature.

### Explicit retry lineage and duplicate handling

[PR #46](https://github.com/werdnum/sudo-service/pull/46) addresses the strongest
remaining signal in the sample: 14 expired requests and 20 explicit retries or
follow-ups. It provides:

- requester resubmission of eligible failed or expired requests;
- immutable predecessor/successor lineage;
- deterministic, concurrent-idempotent retry creation;
- same-requester semantic duplicate detection without cross-requester leakage;
- verified administrator attribution and CSRF protection;
- current-profile preflight before resubmission;
- no automatic retry of a human denial.

### Remove executor resource ceilings

[PR #49](https://github.com/werdnum/sudo-service/pull/49) supersedes the closed
managed-Job proposal in #47. It keeps modest Kubernetes scheduling requests but
removes hard CPU, memory, and default scratch ceilings from executor and init
containers. Server-side execution remains unbounded.

The client gains generic detach behavior, a 12-hour default local wait, and
`--timeout 0` for unlimited waiting. The existing ten-minute Pod **start** guard
still catches unschedulable Pods and broken mounts; post-completion TTL remains
separate from execution duration.

### Read request records directly

[kube-config #1298](https://github.com/werdnum/kube-config/pull/1298) grants the
single practical requester, `k8s-agent-sa`, namespaced `get`, `list`, and `watch`
on `SudoRequest` objects. It grants no Secret access and no write, approval, or
execution permission.

This is the initial observability interface. The agent can answer occasional
questions about expiry, failure, retries, approval latency, images, and command
families directly instead of requiring a speculative product metrics taxonomy.

## Evidence-gated follow-up

### History and retention

A compact requester/admin history surface may still be useful, especially for
retry correlation and an explicit terminal-object retention policy. Do not
assume that a full HTML audit application, CSV export, sensitive raw-command
export, and every filter are required.

Reassess after direct read access has been used in practice. The minimum useful
slice would be bounded recent metadata, requester isolation, retry links, and a
documented retention window.

### Alternate controller namespace

[PR #48](https://github.com/werdnum/sudo-service/pull/48) removes hard-coded
runtime namespace assumptions. It is maintenance and chart-reuse work rather
than a current-cluster product gap. Merge it when reusable installation matters;
it should not displace request usability work.

### Additional metrics

The permission-request regression fixtures shipped in #42. Issue #35 was closed
in favor of direct read access for ad hoc operational analysis. Add a metric
only when it answers a concrete alert, dashboard, or multi-requester capacity
question that cannot be answered economically from request records.

Good future candidates, if demonstrated, are terminal outcome, expiry, retry,
approval latency, notification failure, and output truncation. Avoid requester,
command, UID, target, image, or other high-cardinality/sensitive labels.

## Not currently planned

### Typed administrative special cases

The reviewed sample supported exact failed-Job cleanup and immediate CronJob
runs, but kube-config #1297 removes much of that demand at its source. It did not
establish repeated product need for a special workload-restart operation or a
Secret-key decoder. Secret-related requests generally used credentials inside a
larger scoped API, SSH, or key-derivation workflow without returning the secret.

Do not merge a permanent typed-action CRD/compiler/API surface merely because it
has been implemented. Revisit individual client templates only if new traffic
still repeats the same exact operation after the source fixes.

### Managed Jobs and privileged node diagnostics

PR #47 was closed. The demonstrated long-running failure was an exit-137 memory
ceiling, not a missing server execution timeout. PR #49 removes that ceiling and
makes client waiting optional without adding another execution mode.

Privileged host mounts, root, host PID, and node-diagnostic actions remain out of
scope until repeated requests justify a separately reviewed admission boundary.
Purpose-built automation or an existing Job runner remains preferable for
routine workflows.

### Requester and notification rate limiting

HTTP request submission now requires explicit Kubernetes authorization, and the
sample contained no requester-spam incident. Do not add persistent distributed
token-bucket state, Lease garbage collection, and recovery behavior without a
demonstrated abuse or multi-requester requirement.

### Exact dependency-closure review

Reviewers are not expected to validate exact Nix closures or every package
version. Approval should show the requested action and effective image. Curated
image pins may remain useful operationally, but they are not the product's human
trust boundary.

## Revisit triggers

Add product surface only when current records demonstrate one of the following:

- the same operation is reconstructed repeatedly after its source cause was
  fixed;
- a request fails because the ordinary structured schema cannot express it;
- the reviewer cannot identify a material effect from the permission sentence,
  image, command, and Pod specification;
- direct request reads cannot answer a recurring operational question;
- multiple independent requesters create a real fairness or notification-volume
  problem;
- privileged child workloads repeatedly escape the reviewed request lifecycle.

The next roadmap review should use post-#45/#46/#49 traffic rather than treating
the original 58-request sample as a permanent mandate.
