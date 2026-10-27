# Security and compliance

Status: Draft · 2026-10-26

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
| Malicious WASM plugin | Pinned by digest; `wazero` with no WASI file system, network, clock or random; memory and fuel limits; no host functions beyond the generator ABI. |
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
  "actor": { "type": "user", "id": "alice", "ip": "10.1.2.3", "authTime": "2026-10-26T09:38:55Z" },
  "action": "request.approved",
  "object": { "type": "request", "id": "3b6f2a8c-…", "resource": "infra-live/vm/prod/web01" },
  "details": { "changeSet": "9f2c41…", "rule": "production" },
  "prev": "b1d0…",
  "hash": "7a9e…"
}
```

- `hash = SHA-256(prev || canonical JSON of the event)`. Any modification or deletion breaks the chain.
- The table is written by an insert-only database role; the application role has no `UPDATE` or `DELETE` on it.
- Every hour (and at shutdown) Quai signs a checkpoint (latest `seq` and `hash`) with an Ed25519 key from the secret store. Checkpoints can also be committed to an audit Git repository, giving an external, timestamped anchor.
- Export: syslog (RFC 5424, optionally CEF), OpenTelemetry logs, Splunk HEC, or a webhook. Export is at-least-once with a cursor, so the SIEM can detect gaps.
- `quai audit verify` checks the chain and checkpoints offline.

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
- Retention: requests and audit events are kept for a configurable period, **default 1 year**. Customers with regulatory duties set longer periods (financial institutions often need 10 years); the export to their SIEM or archive is the long-term record. Signed checkpoints are kept for the whole retention period so a pruned chain can still be verified from the last kept checkpoint. Personal data in old records can be pseudonymized without breaking the hash chain by hashing it into the event at write time and storing the clear values in a separate, deletable table.

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
| SBOM and provenance | Syft (CycloneDX), SLSA provenance from the build pipeline, images signed with cosign. On `main`, CI publishes the image it scanned to `ghcr.io/damhau/quai` (tags `main` and `sha-<commit>`), signs it keylessly with the workflow's identity and attests its CycloneDX SBOM (`cosign verify --certificate-identity-regexp 'github.com/damhau/quai/' --certificate-oidc-issuer https://token.actions.githubusercontent.com`). Provenance comes later |
| Secrets in the codebase | gitleaks in CI and as a pre-commit hook |
| Dynamic testing | OWASP ZAP baseline on every release; external penetration test before GA and yearly |
| Disclosure | `SECURITY.md` with a reporting address and response times |
