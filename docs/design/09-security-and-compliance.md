# Security and compliance

Status: Draft · 2026-11-08

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
  personal data) before the latest checkpoint older than the retention whose hash is its event's,
  and the checkpoints before it, by the database's clock ([open questions 36 and
  37](../plan/open-questions.md#still-open)). The worker records `audit.pruned` with the
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
- Export: syslog (RFC 5424, optionally CEF), OpenTelemetry logs, Splunk HEC, or a webhook. Export is at-least-once with a cursor, so the SIEM can detect gaps.

What is recorded, by action (P4-01, P4-02; break-glass, executor calls and configuration changes
come with their features):

| Action | Actor | When |
| --- | --- | --- |
| `auth.login`, `auth.logout` | the person | a sign-in (with the method and the grants it gave), a sign-out |
| `auth.grants_changed` | the person | a sign-in whose grants differ from the person's previous sign-in (`before`, `after`) |
| `token.created`, `token.revoked` | the person | an API token made or revoked (its ID, name, scopes, expiry; never the token) |
| `run_token.created` | the person | a run token generated for a repository (its SHA-256; Quai keeps neither, so revoking it is changing the secret it names, outside Quai's view: [open question 33](../plan/open-questions.md#still-open)) |
| `request.submitted`, `request.cancelled`, `request.retried` | the person, or their token | as named; submitted carries the change set and definitions commit, the reason as a pseudonym |
| `request.reviewed` | the reviewer | every review: the decision, the comment as a pseudonym, the change set |
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

- Personal data is limited to identity attributes from the IdP, reasons and comments people write, and IP addresses in the audit log.
- Self-hosted by default: data stays in the customer's environment. No telemetry leaves the installation unless an admin enables it.
- Retention: requests and audit events are kept for a configurable period, **default 1 year** (`audit.retention` for audit events; [retention](#audit-log)). Customers with regulatory duties set longer periods (financial institutions often need 10 years); the export to their SIEM or archive is the long-term record. Signed checkpoints are kept for the whole retention period so a pruned chain can still be verified from the last kept checkpoint. Personal data in old records can be pseudonymized without breaking the hash chain by hashing it into the event at write time and storing the clear values in a separate, deletable table: pseudonymizing a person deletes their clear values and leaves the hashes ([open question 39](../plan/open-questions.md#still-open)).

## Compliance mapping

Candidate mappings, to be confirmed with each customer's compliance team.

| Control area | How Quai supports it |
| --- | --- |
| Change management (ISO/IEC 27001:2022 A.8.32, SOC 2 CC8.1, FINMA operational resilience requirements, DORA ICT change management) | Every change is requested, reasoned, reviewed against the actual diff and plan, approved by the right people, and traceable to a commit and an apply |
| Segregation of duties (ISO 27001 A.5.3) | No self-approval, distinct approvers, optional definition-author separation, break-glass with mandatory review |
| Access control and reviews (A.5.15–A.5.18) | IdP-based authentication, role scopes, SCIM deprovisioning, exportable access reports for periodic reviews |
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
