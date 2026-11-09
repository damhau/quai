# Security and compliance

Status: Draft · 2026-11-09

## What must be protected

| Asset | Why it matters |
| --- | --- |
| Git write credentials | They can change infrastructure through the customer's pipelines |
| Integrity of approvals | The product's core promise: nothing reaches a protected branch without the required approvals |
| Definitions | They decide what gets written and which rules apply |
| Audit log | Evidence for auditors and incident response |
| Executor credentials | Imperative actions act on live systems directly |
| Personal data | Names, emails, group memberships, reasons people typed |

## Threats and mitigations

| Threat | Mitigation |
| --- | --- |
| Someone satisfies `quai/approval` without Quai | Required check pinned to Quai's identity (GitHub App, ADO authorized identity); hourly verification of branch protection; bypass merges detected and alerted. |
| A malicious definition change (lower approval rules, write elsewhere) | Definitions reviewed through CODEOWNERS; repository floor rules that types cannot remove; locators confined to their repository; every definition change audited; optional separation of duties for definition authors. |
| Injection through form input | No text templating; values are data serialized by format libraries; CEL is not Turing-complete and has no I/O; path params are single safe segments. |
| Path traversal | Locator params validated as segments; every computed path resolved and checked to stay inside the worktree and outside `.git`. |
| ReDoS through schema `pattern` | JSON Schema validation uses Go's `regexp` (RE2, linear time). |
| Malicious WASM plugin | Pinned by digest; `wazero`, a new instance per call, with no WASI file system, network, clock or random; a memory and a time limit per call (`wazero` has no fuel metering); no host functions beyond WASI preview 1, which sees that empty world. |
| Secrets written into Git | Secret inputs are references only (Vault path, ExternalSecret name); rendered output is scanned for secret patterns (gitleaks rules) before commit, and the request is refused on a match. |
| SSRF through webhooks or executors | Outgoing calls only to admin-configured endpoints; allow-lists per executor; no URLs taken from definitions. |
| Webhook spoofing | Per-repository secrets, constant-time comparison, delivery IDs de-duplicated, replay window. Platforms that do not sign payloads (Azure DevOps service hooks) are accepted only on the dedicated webhook host with a secret header, and every event is treated as a notification: Quai re-reads the object from the platform API before acting. |
| Privilege escalation through day-2 actions | Ownership and role checks in one authorization function; executors limited by their own RBAC. |
| Session theft, CSRF, XSS | HttpOnly cookies, CSRF tokens, strict Content Security Policy, React escaping, no `dangerouslySetInnerHTML`, no third-party scripts or CDNs. |
| A stolen or unattended session approves a production change | Step-up: approval rules with `stepUp`, break-glass and (with `auth.stepUp.admin`) repository changes take only a recent, strong enough authentication, from the ID token's `auth_time`, `acr` and `acrs`, checked on the server when the action is made; an API token never steps up; refusals audited ([design 08](08-identity-access-tenancy.md#step-up)). |
| Tampering with the audit log | Append-only table, insert-only database role, hash chain, signed checkpoints, export to SIEM. |
| Supply chain | Small dependency set, `go.sum` and the Go checksum database, `govulncheck`, SBOM, signed images and provenance (see below). |

## Secrets

- Quai stores no secret values in its database. Connections and executors hold references (`vault:…`, `k8s:…`, `env:…`) resolved at use time through the `SecretStore` interface.
- Credentials are cached in memory for at most their TTL and never logged; log fields are filtered by an allow-list, not a deny-list.
- Secret inputs in forms produce references. Writing a secret value into a secret manager on the user's behalf (Vault, with the user's own identity through JWT auth) is a later feature.

## Audit log

Every security-relevant event is an audit event: logins, role changes,
requests, approvals, denials, merges, bypasses, break-glass, definition
changes, executor calls, configuration changes.

```json
{
  "seq": 18234,
  "at": "2026-10-26T09:40:12.331Z",
  "actor": { "type": "user", "id": "pd:5e1c…", "ip": "pd:0b7a…", "authTime": "2026-10-26T09:38:55Z" },
  "action": "request.approved",
  "object": { "type": "request", "id": "3b6f2a8c-…", "resource": "infra-live/vm/prod/web01" },
  "details": { "changeSet": "sha256:9f2c41…", "approvals": ["pd:77d0…"] },
  "prev": "b1d0…",
  "hash": "7a9e…"
}
```

- **The chain.** `hash = SHA-256(prev ‖ JCS(event))`, in hex: `prev` as its 64 hex characters,
  then the event without `prev` and `hash` in RFC 8785's JSON Canonicalization Scheme. The first
  event's `prev` is 64 zeros. `at` is UTC to the millisecond, `authTime` to the second
  ([open question 18](../plan/open-questions.md#still-open)). Any modification or deletion breaks
  the chain at the event changed, or at the one after the event deleted.
- **One at a time, with what it records.** An event is written in the transaction of the change it
  records (`audit.Add` on the transaction `store.Tx` opens), so a change and its event commit or
  roll back together. Just before committing, the transaction takes a transaction-level advisory
  lock, its last lock, and appends its events after the last one: `seq` has no gaps, and the
  lock's holder needs nothing but its own connection to commit and release it.
- **Insert-only.** `quai migrate` connects as the schema's owner (`database.migrateURL`) and grants
  the application role the server and the worker connect as (`database.url`) `SELECT` and
  `INSERT` on the audit tables, nothing more: no `UPDATE`, `DELETE` or `TRUNCATE`, and no change
  to the migrations' records ([open question 19](../plan/open-questions.md#still-open)). When the
  two are one role, `quai migrate` and the server warn, and the audit log page says the database
  does not protect the log.
- **Personal data** (people's names and emails, IP addresses, reasons and comments) is in an event
  as a pseudonym, `pd:` and the SHA-256 of a random salt and the value; the salt and the value are
  in a separate table, `audit_personal`, that pseudonymization deletes from without breaking the
  chain ([data protection](#data-protection)). The API returns the clear values still there with
  each page; an event whose value was deleted shows "(erased)".
- **Reviews** keep the reviewer's authentication time (`reviews.reviewer_auth_time`, none with an
  API token), as do the events of what people do: `actor.authTime` is when they last
  authenticated with the identity provider. An API token's events carry the person and the
  token's ID, without an authentication time. `actor.ip` is the address of the connection Quai
  received, a proxy's behind one ([open question 32](../plan/open-questions.md#still-open)).
- **Checkpoints.** Every hour and when it stops, the worker signs a checkpoint of the latest event,
  when none covers it yet: `{"type": "checkpoint", "seq", "hash", "at", "key", "signature"}`,
  the Ed25519 signature (base64) of the checkpoint's JCS without `signature`, `key` the SHA-256 of
  the public key. The key is in the secret store (`audit.checkpoints.signingKey.ref`); verifying
  needs only its public key, and the keys before a rotation stay valid through
  `audit.checkpoints.previousKeys` ([open question 35](../plan/open-questions.md#still-open)).
  Checkpoints are in `audit_checkpoints`, insert-only like the events, and committed to the audit
  repository when the configuration names one (a connection with delivery mode `direct`), as
  `checkpoints/YYYY/MM/DD/<seq>.json`: an external, timestamped anchor that a restored or
  rewritten database cannot change.
- **Retention.** Events are kept `audit.retention`, a year by default. `quai migrate` stores it as
  the schema's owner; the application role can read it, not change it. Every hour the worker
  calls `audit_prune()`, a `SECURITY DEFINER` function of the owner with its own `search_path`,
  which the application role may execute and nothing more: it deletes the events (and their
  personal data) before the latest checkpoint whose hash is its event's and whose event is older
  than the retention, and the checkpoints before it. An event's age is the database's: `recorded`,
  its clock when it inserted the row, which the application role can neither set nor change, not
  `at`, which the application writes ([open questions 36 and
  37](../plan/open-questions.md#still-open)). `recorded` is not hashed nor exported. The worker records `audit.pruned` with the
  checkpoint the chain now starts from. Without a checkpoint, nothing is pruned.
- **Verification.** One check, offline and in the worker: each event's hash, each link, each
  checkpoint's signature and that its hash is its event's, and that no checkpoint, the audit
  repository's included, is beyond the last event (events lost, as when a database is restored
  from an older backup). A log may start at a checkpoint only when an `audit.pruned` event names
  it. The first `seq` that fails is named, with why. `quai audit export` (or
  `GET /api/v1/audit/export`, for auditors and platform admins) writes the log in JSON lines,
  its checkpoints then its events, pseudonyms and no clear values; `quai audit verify` checks it
  with the public keys and, with `--anchors`, a checkout of the audit repository. The worker
  verifies the log every day with the latest checkpoint of the audit repository, records
  `audit.verified` or `audit.broken`, counts it in `quai.audit.verifications`, logs an error when
  it breaks, and the audit log page shows the last outcome and checkpoint ([open question
  31](../plan/open-questions.md#still-open)). What to do then is the operations guide's
  [runbook](../operations/deployment-and-operations.md#when-the-chain-does-not-verify).
- **Export** to SIEMs: syslog, OpenTelemetry logs and Splunk's HTTP Event Collector, at least once
  and in order, with a cursor per destination, each record with its `seq` and `hash` so the SIEM
  can see a gap and verify the chain ([export to a SIEM](#export-to-a-siem)). A webhook comes in
  Phase 7.

What is recorded, by action (P4-01, P4-02, P4-06, P4-08; break-glass, executor calls and
configuration changes come with their features):

| Action | Actor | When |
| --- | --- | --- |
| `auth.login`, `auth.logout` | the person | a sign-in (with the method and the grants it gave; for a step-up login `stepUp`, and the ID token's `acr` and `acrs` when it has them), a sign-out |
| `auth.step_up_refused` | the person, or their token | an action refused for want of a recent sign-in ([step-up](08-identity-access-tenancy.md#step-up)): what was attempted (`request.review`, `repository.create`, `repository.update`, `repository.delete`, `repository.install_webhook`, `run_token.create`) on which object, and why |
| `auth.grants_changed` | the person | a sign-in whose grants differ from the person's previous sign-in (`before`, `after`): a role change, from the IdP's groups or from `auth.roles` |
| `token.created`, `token.revoked` | the person | an API token made or revoked (its ID, name, scopes, expiry; never the token) |
| `preferences.changed` | the person | their notification preferences changed (P4-08): the choices that differ from the defaults, the digest's hour, and their time zone as a pseudonym; the same preferences again are not a change ([open question 58](../plan/open-questions.md#still-open)) |
| `run_token.created` | the person | a run token generated for a repository (its SHA-256; Quai keeps neither, so revoking it is changing the secret it names, outside Quai's view: [open question 33](../plan/open-questions.md#still-open)) |
| `request.submitted`, `request.cancelled`, `request.retried` | the person, or their token | as named; submitted carries the change set and definitions commit, the reason as a pseudonym |
| `request.reviewed` | the reviewer | every review: the decision, the comment as a pseudonym, the change set, whether the reviewer's authentication satisfied the step-up (`stepUp`) and its `acr` and `acrs` |
| `request.approved`, `request.rejected` | the reviewer, or Quai | the request's state changes; approved names who approved (pseudonyms); Quai when no rule needs approvals or a plan settles them |
| `request.stale`, `request.merged`, `request.applied`, `request.failed` | whoever's action made Quai see it: Quai, a repository's runner, or a person | the request's state changes, with what Quai observed (commit, pull request, run, error); never what a person wrote |
| `request.merged_without_approval` | Quai | a bypass: the pull request merged without Quai's approval |
| `repository.created`, `repository.updated`, `repository.deleted` | the person, or Quai for the configuration's connections | a repository connection changed (its document's SHA-256 and generation) |
| `repository.enforcement_lost`, `repository.enforcement_restored` | Quai | the platform stops or starts enforcing `quai/approval` |
| `definitions.commit` | Quai | the first definitions of a repository, and each commit that changes them: the commit, its author (a pseudonym), the definitions it changes |
| `audit.verified`, `audit.broken` | Quai | the daily verification: the events and checkpoints verified, and where and why it broke |
| `audit.pruned` | Quai | events pruned: how many, and the checkpoint the chain starts from |

Auditors and platform admins read it (`GET /api/v1/audit`, the audit log page under Admin, which
also shows the latest checkpoint and verification, and exports the log):
newest first, a page at a time with a `before` cursor, filtered by action (or its prefix, such as
`request`), actor (a person's name, a repository for its runner, `quai`), object (a request's ID,
a resource, a repository, a token's ID or a person's name), object type and time. An API token
cannot read it.

### Export to a SIEM

The worker sends the audit log to the destinations `audit.export` names (P4-03), each a
`syslog`, `otlp` or `splunk` destination with a name of its own:

- **What is sent.** Each event as the log holds it and `quai audit export` writes it: `seq`, `at`,
  `actor`, `action`, `object`, `details`, `prev` and `hash`, with personal data as pseudonyms and
  never the clear values of `audit_personal` ([open question 40](../plan/open-questions.md#still-open)).
  The SIEM sees a gap or a repeat from `seq`, and checks each record's `hash` and its link to the
  one before with `quai audit verify`, once its copy is de-duplicated by `seq`. Checkpoints are not
  sent: the audit repository holds them.
- **At least once, in order.** `audit_export_cursors` holds, per destination, the `seq` and `hash`
  of the last event it took. Every 30 seconds, and at its start, the worker sends each destination
  the events after its cursor in `seq` order, in batches of 500 for at most a minute, one worker at
  a time per destination, and moves the cursor once the destination took a batch. A batch it did
  not take, or took without the cursor moving (the worker stopped between the two), is sent again
  at the next run: a destination that was down catches up with no gap, and may get a batch twice.
  Taken means: for syslog, which has no acknowledgment, that the receiver closed the connection
  after reading the batch, which the worker waits for after closing its own side; for OTLP, a
  `200` (a partial success moves the cursor past the records the collector rejected, which OTLP
  says not to send again, and counts as a failure); for HEC, a `200`, without indexer
  acknowledgment ([open question 41](../plan/open-questions.md#still-open)). Events commit in
  `seq` order and without gaps (one at a time, above), so reading after the cursor misses none.
- **What breaks the order.** A new destination, or one renamed, starts from the oldest event the
  log holds. Events pruned before a destination took them are a gap: the worker logs an error,
  and pruning does not wait for the export ([open question 43](../plan/open-questions.md#still-open)).
  An event after the cursor whose `prev` is not the cursor's `hash` means the log changed since it
  was exported (a restore from an older backup, a rewrite): the worker logs an error and sends it,
  and the SIEM sees the break as well. The cursors are not audit tables: the application role
  writes them, so a cursor moved forward makes a gap the SIEM sees.
- **Transport.** TLS is always verified, against the system's certificates and `network.caFile`;
  nothing turns verification off. A syslog `transport: tcp` or an `http://` URL is unencrypted,
  which production refuses except to a loopback address, a collector or relay on the same host
  ([open question 42](../plan/open-questions.md#still-open)). HTTP destinations go through the
  proxy, syslog does not; a redirect is an error, never followed, so credentials go nowhere else.
  The HEC token and the OTLP headers are secret references, resolved when sending, never logged
  nor in an error.
- **Syslog** (RFC 5424 over TLS, RFC 5425, or TCP; framed by octet counting, RFC 6587), a
  connection per batch: `PRI` the facility (13, log audit, by default) and the severity, warning
  (4) for `audit.broken`, `request.merged_without_approval` and `repository.enforcement_lost`,
  notice (5) otherwise; `TIMESTAMP` the event's `at`; `HOSTNAME` the worker's; `APP-NAME` `quai`;
  `MSGID` the action; no structured data; `MSG` the record in JSON, its non-ASCII characters
  escaped, so with no BOM. A record can exceed RFC 5425's 2048-octet minimum: the receiver must
  take larger messages. CEF is not built.
- **OpenTelemetry logs** (OTLP/HTTP, binary protobuf, to the endpoint's `/v1/logs` when it names
  no path): a resource with `service.name`, `host.name` and `service.version`, the scope
  `quai.audit`, and a log record per event: its time the event's `at`, severity `INFO2` (notice)
  or `WARN` as above, its body the record in JSON, and the attributes `quai.audit.seq`,
  `quai.audit.hash` and `quai.audit.action`.
- **Splunk HEC** (`/services/collector/event`, `Authorization: Splunk <token>`): an event per
  record, its `time` the event's `at` (epoch seconds and milliseconds), `host` the worker,
  `source` `quai`, the configured `index` and `sourcetype`, and the record as `event`.

The worker counts each run in `quai.audit.exports` (by destination and outcome) and reports each
destination's lag, `quai.audit.export.lag`, the age of the oldest event it has not taken by the
database's clock, and their number, `quai.audit.export.pending`; the chart's alert fires when a
destination lags ([the runbook](../operations/deployment-and-operations.md#when-the-audit-export-lags)).

### Evidence bundle

For each request, a downloadable bundle (JSON plus a PDF summary): the request
and reason, the diff, definitions commit, plan summary and policy results,
approvals with authentication times, merge commit, apply result, and the audit
events with their hashes. It is what an auditor asks for in a change-management
sample, and what Quai attaches to ITSM change requests.

## Commit signing and attribution

- GitHub: commits created through the GitHub App API are signed by GitHub and shown as verified.
- Azure DevOps and other platforms: commits are signed with an SSH or GPG key held in the secret store, so history can be verified offline; where the platform supports signature verification, the public key is registered there. Azure Repos does not display signature verification at the time of writing.
- The commit author is the requester; trailers carry the request ID, change set hash and stack. Approvers are recorded in the merge commit message or on the PR.

## Data protection

- Personal data is limited to identity attributes from the IdP, reasons and comments people write, and IP addresses in the audit log. The identity attributes are kept in the directory (`users`: name, verified email, groups, the IdP's `sub` and `oid`, last login), which nothing erases yet ([open question 44](../plan/open-questions.md#still-open); the operations guide gives the statement).
- Self-hosted by default: data stays in the customer's environment. No telemetry leaves the installation unless an admin enables it.
- Retention: requests and audit events are kept for a configurable period, **default 1 year** (`audit.retention` for audit events; [retention](#audit-log)). Customers with regulatory duties set longer periods (financial institutions often need 10 years); the export to their SIEM or archive is the long-term record. Signed checkpoints are kept for the whole retention period so a pruned chain can still be verified from the last kept checkpoint. Personal data in old records can be pseudonymized without breaking the hash chain by hashing it into the event at write time and storing the clear values in a separate, deletable table: pseudonymizing a person deletes their clear values and leaves the hashes ([open question 39](../plan/open-questions.md#still-open)).

## Compliance mapping

Candidate mappings, to be confirmed with each customer's compliance team.

| Control area | How Quai supports it |
| --- | --- |
| Change management (ISO/IEC 27001:2022 A.8.32, SOC 2 CC8.1, FINMA operational resilience requirements, DORA ICT change management) | Every change is requested, reasoned, reviewed against the actual diff and plan, approved by the right people, and traceable to a commit and an apply |
| Segregation of duties (ISO 27001 A.5.3) | No self-approval, distinct approvers, optional definition-author separation, break-glass with mandatory review |
| Access control and reviews (A.5.15–A.5.18) | IdP-based authentication, role scopes, role changes in the audit log, SCIM deprovisioning, the access report (every person, their groups, teams, roles by scope and the group that gives each, their live API tokens; a page and a CSV) for periodic reviews ([directory](08-identity-access-tenancy.md#directory)) |
| Logging and monitoring (A.8.15, A.8.16) | Hash-chained audit log, SIEM export, alerts on bypass and break-glass |
| Outsourcing and data location (FINMA outsourcing rules, Swiss nFADP, GDPR) | Self-hosted, no outbound telemetry, data residency under customer control |

## Security engineering

| Practice | Tooling |
| --- | --- |
| Static analysis | `go vet`, `staticcheck`, `gosec`, CodeQL; `eslint` and `tsc` for the frontend |
| Vulnerable dependencies | `govulncheck`, `npm audit` with lockfile, Renovate with grouped updates and a cool-down period before adopting new versions. CI fails on what `govulncheck` finds reachable and on `npm audit` findings of high severity, development dependencies included; a vulnerable transitive package is pinned to its fixed version with an npm `overrides` entry until its parent catches up |
| Images | Distroless or UBI-minimal base, non-root, read-only root file system; scanned with Trivy. For now Alpine (deployment and operations, Packaging); every build fails on HIGH or CRITICAL findings that have a fix |
| SBOM and provenance | Syft (CycloneDX), SLSA provenance from the build pipeline, images signed with cosign. On `main`, CI publishes the image it scanned to `ghcr.io/damhau/quai` (tags `main`, `sha-<commit>` and `main-<commit time, UTC, YYYYMMDDHHMMSS>-<short commit>`, which sorts by date for Argo CD Image Updater), signs it keylessly with the workflow's identity and attests its CycloneDX SBOM (`cosign verify --certificate-identity-regexp 'github.com/damhau/quai/' --certificate-oidc-issuer https://token.actions.githubusercontent.com`). Provenance comes later |
| Secrets in the codebase | gitleaks in CI and as a pre-commit hook |
| Dynamic testing | OWASP ZAP baseline on every release; external penetration test before GA and yearly |
| Disclosure | `SECURITY.md` with a reporting address and response times |
