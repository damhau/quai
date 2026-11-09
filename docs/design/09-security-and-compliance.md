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
| A malicious definition change (lower approval rules, write elsewhere) | Definitions reviewed through CODEOWNERS; repository floor rules that types cannot remove; locators confined to their repository; every definition change audited; optional separation of duties for definition authors (`separationOfDuties.definitionAuthors`): whoever changed a request's definitions in the period before it cannot approve it ([separation of duties](05-delivery-and-approvals.md#separation-of-duties)). |
| A definition author approves requests planned with their own change | Every identity of the commits that changed the request's type or blueprint files counts: author, committer, `Co-authored-by` trailers, the committer of a merge bringing them in; each matched by the directory's verified emails only ([question 30](../plan/open-questions.md#still-open)), case-insensitively, and an email that matches no one yet still excludes whoever has it verified when reviewing. The period is measured from the definitions Quai itself recorded, never from commit dates, which their authors write; with no record that old, the whole history counts. Computed when the request is planned and kept with it, so a later commit or a history rewrite changes nothing; stricter options make a pending request stale ([questions 71 to 74](../plan/open-questions.md#still-open)). **Limits:** Git identities are self-asserted. A forged identity can at worst block its victim from approving (the safe direction); a definition author who commits under an identity that matches no one in Quai (another email, a bot) is not excluded, and the request lists that identity. Quai does not read the platform's records (who pushed, who opened or merged the pull request) yet; until it does, CODEOWNERS on `.quai/` and the audited `definitions.commit` events are the control over who changes definitions. |
| A destroying change is approved by one person | `destroys: requireTwo`: two different people, by name, never the requester or a definition author, among the request's approvers, whatever `distinct: false` says; it applies until a plan shows that nothing is destroyed, so no plan yet, a stack without a plan, a failed plan and direct delivery all need two; each new plan decides again ([question 73](../plan/open-questions.md#still-open)). |
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
| A link in a chat message approves for someone else, or approves content that changed | A link is not a credential: it opens the request with a decision ready, and the decision is the authenticated, CSRF-protected review with all its checks (who may review, step-up, the change set). The link adds its own: HMAC-SHA256 with a dedicated key, a day's lifetime, bound to the request, the approver, the decision and the change set hash; refused for anyone else, once the request is decided or its content changes, and never with an API token; refusals audited ([approving from chat](11-integrations.md#approving-from-chat)). |
| Forged or replayed requests to Slack's interaction endpoint (`POST /slack/interactions`, the one endpoint a third party calls without a session or a token) | Verified before anything else: the body read within 128 KiB, the signature `v0=` HMAC-SHA256 of `v0:<timestamp>:<body>` with the app's signing secret compared in constant time, the timestamp within five minutes of the server's clock either way, each header sent once and well formed; no payload parsed, database touched or person looked up before. The endpoint decides nothing: a verified request is acknowledged (200, no body) and its payload is not read; Slack's user, team or name is never a Quai identity, and approving stays the signed link, a Quai session and the review. A replay within the five minutes therefore only gets another acknowledgement: no nonce store. Every refusal gets the same 401 with no body, logged with the reason and the client's address, never the body or the signature; requests are rate limited (20 a second, bursts of 40, 429 beyond) before any work. Only that exact path skips sessions and CSRF tokens (not a path under it, nor one that decodes to it); it is absent without a signing secret, and allowed on the webhook host ([question 67](../plan/open-questions.md#still-open)). |
| A person's text acts in a Slack message: `<!channel>`, `<@U…>`, `<https://evil\|click>` | Block Kit built from Go structs with `encoding/json`; every text in a block a `plain_text` object, which Slack does not format; the fallback text, which Slack formats, with `&`, `<` and `>` escaped; links do not unfurl. Channel posts carry identifiers, the state and a link only ([question 64](../plan/open-questions.md#still-open)). |
| A Slack account that is not the person's gets their approval messages | Looked up with the verified email from the directory only; written to only when the account is an active full member of the app's workspace (or its Enterprise organization) with that email: not a guest, a bot, an account of another workspace or one whose email is not confirmed ([question 66](../plan/open-questions.md#still-open)). The message's links open nothing without the approver's own Quai session. |
| The Slack bot token leaks | A secret reference, resolved at use; sent only in the `Authorization` header to the configured https Web API through `network` with TLS verified, never to an address Slack sends (`response_url` is ignored); never in a URL, a body, an error or a log. |
| An overage claim or a Graph answer sends Quai, with the person's token, to another host | The endpoint `_claim_sources` names is ignored; Quai calls only `auth.graph.baseURL` (https, configured), through `network` with TLS verified, follows no redirect and no `@odata.nextLink` (a partial answer refuses the login); the ID token's `tid` must be `auth.graph.tenantId` first. The token is the sign-in's own, delegated, for `/me` only, in memory during the callback, never stored, logged or in an error ([question 68](../plan/open-questions.md#still-open)). |
| A person in too many groups for the ID token signs in without them, and escapes a restriction | A group overage is resolved from Microsoft Graph or the login is refused: an error, throttling, a refused token or permission, a partial answer or more than 10,000 groups refuses it. Bounded: 1 MiB, two retries, five seconds' `Retry-After`, twenty seconds in all. |
| Someone approves as a requester's manager without being it | The manager is an immutable identifier (`oid`, `sub`) from the identity provider or Graph, never a name or an email, resolved to exactly one person of the directory, otherwise no manager and the rule cannot be satisfied; a failed lookup clears the manager rather than keep a stale one; the requester never approves as their own manager; the manager is kept with the request at submission, so a later change moves nothing; a change of manager is audited ([questions 69 and 70](../plan/open-questions.md#still-open)). |
| A delegation approves for someone who did not hand on their approvals, or more than they could: made for someone else, with a stolen token, passed along a chain, or counted twice | Only the delegator makes one, for themselves, from their session (the person from the session, never from the path or the body); an API token can neither make one nor review on someone's behalf; a step-up whenever `auth.stepUp` is configured. The delegate signed in and is active, the period is at most 90 days and never in the past, one delegate at a time, no one delegates to someone who delegated their own (no cycles), and a delegate holds the delegator's own rights only (no chains). Rights are judged at the review, with the delegation read again under lock: the delegator could review it then, and neither the delegator nor the delegate requested it or is an excluded definition author. A review on someone's behalf is both people's: one human makes one approval, for `distinct` and `requireTwo` too. The review names both, with the delegation, on the request, in the audit log and in the trailers, whose values are one line without control characters ([delegation](05-delivery-and-approvals.md#delegation), [open questions 24, 75 and 76](../plan/open-questions.md#still-open)). |
| An expired request approved anyway, or its pull request closed, commented on or reopened wrongly: an approval racing the expiry, a sweep run twice, a type changed to expire or spare waiting requests, a pull request found by its branch's name, or a reminder that tells too much | The sweep decides each step under the request's row lock, on its state read again, and marks it in the step's transaction; reviews take the same lock, so whichever commits first wins and the other does nothing (a review of an expired request gets 409). Expired is final, its reviews voided (`voided_at`) and never counted again. The expiry is fixed at submission from the definitions then, and counted from the submission by Quai's clock, never from a commit date. Quai fails `quai/approval`, comments and closes only the pull request it opened and recorded, never one found by its branch; the comment is said once (a marker found again among the comments, and an event) and carries the request's number, its expiry and its link, nothing its readers could not already see. A pull request merged meanwhile is a bypass (alerted); one closed by someone else is left alone. Reminders go only to the people the request waits for (never its requester or an excluded definition author); an escalation contact the request does not wait for gets its identifiers only ([reminders, escalation, expiry](05-delivery-and-approvals.md#reminders-escalation-expiry), [open questions 78 to 82](../plan/open-questions.md#still-open)). |
| A change merges outside its change windows: a request approved before a freeze, a window decided at approval and not at merge, a repository document lifting a freeze of the configuration, a condition that fails read as open, a platform asked to merge early, a sweep run twice | The decision is taken at merge time, under the request's row lock and again under its merge lock right before Quai asks the platform, with the windows as stored then; one function moves a request to merging, and the delivery around it checks everything it did before. Every window that applies must be open, so a repository document only adds constraints. A `when` that fails, or windows that cannot be read, hold the request and say why. Outside its windows `quai/approval` is pending with the reason and nothing is asked of the platform (no auto-merge, auto-complete, merge queue or group approval); the sweep holds requests whose windows close, and a merge on the platform while Quai holds a request is a bypass (alerted). The `when` sees params and the request's kind, action, type, blueprint and repository, never its reason. Residual: a merge the platform has under way when a window closes can land within the minute it takes Quai to turn the check pending ([change windows and freezes](05-delivery-and-approvals.md#change-windows-and-freezes), [open questions 89 to 93](../plan/open-questions.md#still-open)). |
| A ServiceNow approval counts for what it did not approve: another change request (made by someone, found by number or text), an approval of earlier content, a value read as approved that is not, a reading racing a content change; or Quai's text acts in ServiceNow, its credentials leak, ServiceNow sends Quai elsewhere | Quai reads only the change request it created, by the sys_id ServiceNow answered, never a number or a text; its correlation ID carries 64 random bits so that no one makes one first, a lost creation is found by it, two are ambiguous and taken for nothing, a reading with another is pending. A change request is for one change set: a change of content supersedes it (a work note) and opens a new one; its approval never counts for other content. Only `approval` approved with the state Scheduled or Implement approves; a value ServiceNow does not document is pending and logged. A reading is applied under the request's row lock, to the change request current for the content, through the policy and the one path to *Approved*, the gate and the merge; Quai's other rules still apply. Every text Quai writes has `[code]` tags broken and HTML escaped, and is cut to its field's length; work notes carry identifiers and counts, never a plan's values, a break-glass's reason or another request's data. Calls go only to the configured https instance through `network` with TLS verified, no redirect followed, nothing from an answer called (the page's link is made from the configured root and a checked sys_id), bounded answers and timeouts; the secret, password and token are secret references, never logged or in an error. Unreachable, nothing is approved and nothing lost ([ITSM](05-delivery-and-approvals.md#itsm), [questions 107 to 111](../plan/open-questions.md#still-open)). **Residual:** whoever may approve in ServiceNow approves for Quai's rule: ServiceNow's own access control decides who that is |
| ServiceNow's closing or a break-glass's incident abused: the evidence attached to a change request Quai did not create, or of other content; a file planted under Quai's name; a retry attaching or moving twice; the attachment telling more than its readers may see; an incident reference that injects into an encoded query or a link; an emergency held by ServiceNow; the reason sent to it | Quai closes and attaches only to the change request whose sys_id it stored, the request's current one, for the change set that landed; a superseded one is never touched, and one whose correlation ID is no longer Quai's gets nothing. The attachment is the bundle without its proof, as an API token downloads it, and its PDF: the request's own events, no other request's data, no secret, IP address or plan value. Its names are Quai's (`quai-evidence-<number>-<64 random bits>`), never from a person's text; each file is at most 10 MiB, its size and SHA-256 recorded before the upload, its sys_id after, and a retry looks it up by its name before uploading; each state move is taken from the state ServiceNow reports. Uploads go through the same client: the configured https instance, `network` with TLS verified, no redirect, bounded answers, credentials never in an error. A refusal is a problem on the page and a warning, never retried forever. The incident must be `^[A-Z]{2,10}[0-9]{1,16}$` before any call, so no `^`, `=`, `@` or operator word reaches a query; it is queried as `number=<it>` only, the answer filtered for that exact number, two matches ambiguous. The check runs before the break-glass's transaction, with a 10-second bound, never under the row lock; unreachable lets the break-glass through, recorded as not verified and asked again, a later "no" a problem on the review and an audit event, nothing undone. The incident's link is made from the configured root and a checked sys_id; the reason never reaches ServiceNow, and every text goes through `servicenow.Text` ([ITSM](05-delivery-and-approvals.md#itsm), [questions 115 to 122](../plan/open-questions.md#still-open)). **Residual:** a closed change request is ServiceNow's record: its users may still edit or delete the attachments; the bundle's hashes, checked against an export, show it |
| The SCIM token leaks, or something else reaches SCIM: SCIM deactivates people and decides their groups, platform admins' included | A credential of its own, never an API token or a session, nor taken by the rest of the API: `quai_scim_` and 256 random bits, made outside Quai (`quai scim-token`); Quai keeps only its SHA-256, in a secret reference, compares it in constant time with each configured token, and refuses a header sent twice. Rotation by a second token, revocation by removing it or changing its secret (within a minute); its last use is in the access report, and every change it makes is in the audit log as the provisioning's, by token name. Rate limited before any work (50 a second, bursts of 100); bounded bodies (1 MiB), types, strings, operations, members and pages; filters parsed by a small strict parser to query parameters, never SQL; errors never carry an internal error. Only `/scim/` exactly (not a path that decodes to it) skips sessions; it is allowed on the webhook host. **Residual:** the rate limit is shared, so a flood of bad requests can slow the real provisioning, which retries ([design 08](08-identity-access-tenancy.md#scim-provisioning), [question 83](../plan/open-questions.md#still-open)). |
| SCIM grants a privilege, or binds someone to another person's account | SCIM never gives a group the ID token does not: a provisioned person's groups are their last sign-in's, less the groups SCIM provisioned that do not list them, and sessions and tokens act with the groups they were issued with that the person still has, checked at every request, so a removal from the admin group takes effect at once. People are bound by `externalId`, the identity provider's immutable `oid` (or `sub`), never by a name or an email: adoption of someone who signed in needs matching identifiers; a sign-in whose identifier or name disagrees with SCIM's is refused; a rename keeps the person; a deleted person's name stays theirs, so a new person given it is refused rather than inheriting requests, reviews, delegations or groups; `externalId` never changes. Reserved names (break-glass accounts) and development users are out of SCIM's reach. Every membership and group change is audited with the groups and grants before and after ([questions 84, 85 and 88](../plan/open-questions.md#still-open)). |
| A leaver keeps acting: an open session, an API token, a delegation, a pending approval, a chat link | A SCIM deactivation acts at once: the session epoch moves, so sessions end at their next request; API tokens and delegations, given and received, are revoked; requests not delivered are cancelled; their reviews, and reviews on their behalf, of requests still waiting are voided and the approvers notified again; chat links need their session; nothing is sent to them; their sign-ins are refused. Reactivation restores none of it ([question 86](../plan/open-questions.md#still-open)). A deactivation by hand (the database) refuses sessions and tokens while it lasts. |
| Break-glass abused: a person merges their own change, or one that changed after they looked, with a stolen session or a token; it bypasses more than Quai; its record lost or its review closed early; its reason injects markup | Its own role, never implied by platform admin; never the requester, the requester's delegate or a definition author the separation excludes; a browser session only (tokens refused, CSRF), a step-up whenever `auth.stepUp` is configured, a deactivated person refused. The call carries the change set and head the page showed, refused when they changed; it goes through the one delivery (the gate, the merge lock, the head pinned) and lifts only Quai's approvals and change windows, never the platform's protection, other checks or `waitForChecks`. The record, its review, the audit event and the alert are written with the state change, under the row lock, before any platform call, so its merge is not taken for a bypass and survives a crash. Reason and incident bounded, control, line-separator and bidirectional characters refused; the reason never reaches the platform or the alerts, and is shown as text; `quai/approval` says "Break-glass by <name>" within 140 characters. Alerts at once to security and the owning team, on every channel, that preferences cannot mute; a review closed only by two different reviewers, never the breaker or the requester, never withdrawn, reminded daily ([break-glass](05-delivery-and-approvals.md#break-glass), [questions 94 to 99](../plan/open-questions.md#still-open)). |
| Tampering with the audit log | Append-only table, insert-only database role, hash chain, signed checkpoints, export to SIEM. |
| An evidence bundle forged or edited, or one that tells its reader more than they may see | A bundle carries the request's events as the log holds them and the segments of the log up to signed checkpoints; `quai audit verify --bundle`, with the public key the auditor holds, checks every hash, link and signature, and the summary against the events: a changed byte, a removed event, a swapped checkpoint or an edited summary field fails, by name. The proof (other requests' events, with pseudonyms) goes only to auditors of the whole platform and platform admins, in a session; others, and every API token, get the request's own events. IP addresses, emails, plan values and secrets are never in a bundle. Downloads go through authorization and are audited; the sample's filters are validated and the archive bounded (50 requests, 256 MiB, two at once), its names made from request numbers. The PDF draws plain text without links or scripts, and clips hostile input. **Residual:** a bundle cut down to fewer events and segments still verifies; the full export is the completeness check ([evidence bundle](#evidence-bundle)). |
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

What is recorded, by action (P4-01, P4-02, P4-06, P4-08, P4-09, P4-11, P4-13, P4-15, P4-17, P4-18; executor calls and
configuration changes come with their features):

| Action | Actor | When |
| --- | --- | --- |
| `auth.login`, `auth.logout` | the person | a sign-in (with the method and the grants it gave; for a step-up login `stepUp`, and the ID token's `acr` and `acrs` when it has them), a sign-out |
| `auth.step_up_refused` | the person, or their token | an action refused for want of a recent sign-in ([step-up](08-identity-access-tenancy.md#step-up)): what was attempted (`request.review`, `request.break_glass`, `request.break_glass_sign_off`, `delegation.create`, `repository.create`, `repository.update`, `repository.delete`, `repository.install_webhook`, `run_token.create`) on which object, and why |
| `auth.grants_changed` | the person | a sign-in whose grants differ from the person's previous sign-in (`before`, `after`): a role change, from the IdP's groups or from `auth.roles` |
| `auth.manager_changed` | the person | a sign-in whose manager differs from the directory's (`before`, `after`: the manager's identifier as a pseudonym, where it came from, and why there is none), including a manager cleared by a failed lookup |
| `token.created`, `token.revoked` | the person; the provisioning for a token revoked by a deactivation | an API token made or revoked (its ID, name, scopes, expiry; never the token); revoked by a deactivation, the reason and the owner as a pseudonym |
| `scim.user_created`, `scim.user_changed`, `scim.user_deactivated`, `scim.user_reactivated`, `scim.user_deleted` | the provisioning (type `scim`, the SCIM token's name) | a person provisioned ([SCIM](08-identity-access-tenancy.md#scim-provisioning)): created (whether an existing person was adopted, active, the manager's SCIM ID), changed (`before`, `after`: the `userName` and the manager as pseudonyms), deactivated (sessions ended, how many tokens revoked), reactivated, deleted (deactivated and unbound) |
| `scim.group_created`, `scim.group_changed`, `scim.group_deleted`, `scim.members_changed` | the provisioning | a provisioned group: its `displayName` and `externalId` (before and after a change); members added and removed, as pseudonyms; deleted, the members it had |
| `auth.groups_changed` | the provisioning | a provisioned person's groups changed by SCIM (`before`, `after`, and the grants they give, `grantsBefore`, `grantsAfter`): effective at their next request, in sessions and tokens |
| `preferences.changed` | the person | their notification preferences changed (P4-08): the choices that differ from the defaults, the digest's hour, and their time zone as a pseudonym; the same preferences again are not a change ([open question 58](../plan/open-questions.md#still-open)) |
| `delegation.created`, `delegation.revoked` | the person: the delegator, or a platform admin revoking it; the provisioning when it deactivates either person | approvals handed to a colleague (P4-13): the delegator and the delegate as pseudonyms, the period; revoked carries who revoked it (`by`: `delegator`, `admin` or `deactivation`) ([delegation](05-delivery-and-approvals.md#delegation)) |
| `run_token.created` | the person | a run token generated for a repository (its SHA-256; Quai keeps neither, so revoking it is changing the secret it names, outside Quai's view: [open question 33](../plan/open-questions.md#still-open)) |
| `request.submitted`, `request.cancelled`, `request.retried` | the person, or their token; the provisioning for a request cancelled because its requester was deactivated | as named; submitted carries the change set and definitions commit, the approval expiry and when it falls (`expiry`, `expiresAt`) when the request waits for approval, the reason as a pseudonym, and the repository's separation of duties when it has one (`separationOfDuties`: the options, when the period began, the commit it began from, the people excluded with their commits and the identities that matched no one, people and identities as pseudonyms) |
| `request.reviewed` | the reviewer | every review: the decision, the comment as a pseudonym, the change set, whether the reviewer's authentication satisfied the step-up (`stepUp`) and its `acr` and `acrs`; for a review on someone's behalf, the delegator (`onBehalfOf`, a pseudonym) and the delegation's ID |
| `request.link_refused` | the person | a link from a chat card (Teams, Slack) refused, when opened or with a review ([approving from chat](11-integrations.md#approving-from-chat)): why (`code`: `malformed`, `signature`, `expired`, `request`, `approver`, `decision`, `decided`, `changed`, `session`, and the reason), and, when its MAC holds, the link's channel, decision, change set, expiry, whom it was sent to and on whose behalf (`onBehalfOf`), as pseudonyms; never what an unverified link says ([open question 62](../plan/open-questions.md#still-open)) |
| `request.approved`, `request.rejected` | the reviewer, or Quai; the system `servicenow` when a change request decides | the request's state changes; approved names who approved (pseudonyms), and for approvals given on someone's behalf the approver, the delegator and the delegation (`onBehalfOf`); Quai when no rule needs approvals or a plan settles them; decided by ServiceNow, the change request (`changeRequest`: its number, sys_id, kind, correlation ID, change set, state, approval and outcome) |
| `request.change_request_opened`, `request.change_request_superseded` | Quai | a request's change request in ServiceNow (P4-19): opened (its number, sys_id, kind, template, correlation ID, the change set it is for), or superseded because the request's content changed ([ITSM](05-delivery-and-approvals.md#itsm)) |
| `request.approval_withdrawn` | the system `servicenow` | an approved request back to pending approval because ServiceNow no longer reports its change request approved: the change set and the change request as ServiceNow reported it |
| `request.change_request_closed` | Quai | Quai is done closing a request's change request (P4-20): its number, sys_id, kind, correlation ID and change set, the close code wanted (`outcome`: `successful`, `unsuccessful`), how it ended (`result`: `closed`, `already` closed or canceled in ServiceNow, `left` to its change manager, `refused`), the state ServiceNow reported, the files attached (name, bytes, SHA-256, sys_id) and the problem when there is one |
| `request.break_glass_incident` | the system `servicenow` | ServiceNow answered, after a break-glass went ahead with its incident not verified (P4-20): the incident, the answer (`check`: `verified` with its `sysId`, `refuted`, or `unverified` when the answer is unclear) and the problem |
| `request.incident_linked` | Quai | a break-glass's incident linked to the request's change request (P4-20): the incident and its sys_id, the change request and its sys_id, and `link`: `linked` (its `rfc` set), `kept` (it named another change request already) or `refused`, with the problem |
| `request.reminded`, `request.escalated` | Quai | a request still waiting for approval (P4-14): how long after its submission (`after`); reminded names the people reminded (`approvers`, with `onBehalfOf` for a delegate, as pseudonyms), escalated the escalation's entries (`to`: the groups, and the users as pseudonyms) ([reminders, escalation, expiry](05-delivery-and-approvals.md#reminders-escalation-expiry)) |
| `request.reviews_voided` | the provisioning | a deactivated person's reviews of a request still waiting for approval, theirs and those on their behalf, voided (`approver`, a pseudonym, and how many reviews); the approvers are notified again |
| `request.expired` | Quai | a request waited for approval longer than its expiry: the expiry, when it was submitted and when it expired, and how many reviews it voided (`voidedReviews`) |
| `request.pull_request_closed` | Quai | Quai closed a request's pull request (`pullRequest`), and why (`reason`: `expired`), recorded before it closes it |
| `request.window_waiting`, `request.window_opened` | Quai | an approved request held for its change windows (P4-16): the state it was in (`from`: `approved`, or `merging` when a window closed before it merged), the windows that hold it, when they open (`opens`) or why they cannot be decided (`error`); released, the windows that apply and when they close (`closes`) |
| `request.break_glass` | the person | a request merged by break-glass (P4-17): the state it was broken from (`from`), the change set and head it was broken for, the incident, the reason as a pseudonym, whether the authentication satisfied the step-up (`stepUp`) and its `acr` and `acrs`, and whether it lifted a change window's hold (`windowHeld`); with `itsm.servicenow`, what ServiceNow said of the incident (`incidentCheck`: `verified`, with the incident's `incidentSysId`, or `unverified`) ([break-glass](05-delivery-and-approvals.md#break-glass)) |
| `request.break_glass_signed_off` | the reviewer | a sign-off of a break-glass's post-incident review: the capacity (`platform-admin`, `auditor`, `owner`), the comment as a pseudonym, the step-up, and how many sign-offs it has of how many needed |
| `request.break_glass_review_closed` | the reviewer of the second sign-off | the review closed: who signed it off (pseudonyms), the incident, when glass was broken |
| `request.break_glass_reminded` | Quai | an open review reminded (every 24 hours): the sign-offs it has and needs |
| `request.stale`, `request.merged`, `request.applied`, `request.failed` | whoever's action made Quai see it: Quai, a repository's runner, or a person | the request's state changes, with what Quai observed (commit, pull request, run, error); never what a person wrote; a merge of a broken request carries `breakGlass` |
| `request.merged_without_approval` | Quai | a bypass: the pull request merged without Quai's approval |
| `request.merged_outside_window` | Quai | a bypass: the pull request merged while Quai held the approved request for its change windows (`from`, `commit`, `pullRequest`) |
| `repository.created`, `repository.updated`, `repository.deleted` | the person, or Quai for the configuration's connections | a repository connection changed (its document's SHA-256 and generation) |
| `repository.enforcement_lost`, `repository.enforcement_restored` | Quai | the platform stops or starts enforcing `quai/approval` |
| `definitions.commit` | Quai | the first definitions of a repository, and each commit that changes them: the commit, its author (a pseudonym), the definitions it changes |
| `audit.verified`, `audit.broken` | Quai | the daily verification: the events and checkpoints verified, and where and why it broke |
| `audit.pruned` | Quai | events pruned: how many, and the checkpoint the chain starts from |
| `evidence.downloaded` | the person, or their token | an evidence bundle downloaded (P4-18), about the request (object type `evidence`, its ID) or `archive` for a sample: the format (`json`, `pdf`, `zip`), the requests and how many (`count`), and whether it carried the proof (`proof`) ([evidence bundle](#evidence-bundle)) |

Slack's interaction requests are not audited: they carry no Quai identity and change nothing, and
anyone can send one. Refusals are logged (`a Slack interaction request was refused`, with the reason
and the client's address), never with their body or signature.

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
  (4) for `audit.broken`, `request.merged_without_approval`, `request.merged_outside_window`,
  `repository.enforcement_lost` and `request.break_glass`, notice (5) otherwise; `TIMESTAMP` the event's `at`; `HOSTNAME`
  the worker's; `APP-NAME` `quai`;
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

For each request, a download (P4-18): `GET /api/v1/requests/{id}/evidence`, a JSON bundle, or
with `?format=pdf` its PDF summary. It is what an auditor asks for in a change-management sample,
and what Quai attaches to ITSM change requests, without its proof (built in P4-20: [ITSM](05-delivery-and-approvals.md#itsm)). The request page offers both to whoever
may download them; the *Evidence* page lists a period's requests for auditors and downloads a
sample of them as one archive.

**What it holds.** One JSON document, `bundleVersion` 1, its keys in a fixed order, made from one
read-only snapshot of the database, so the same state gives the same bytes but `generatedAt`:

- `request`: its number, kind, action, type or blueprint, repository, target, stack, requester,
  reason, state, inputs, when it was created, submitted and last changed, the definitions and base
  commits, the approval expiry, its owners, its approval rules as its page shows them (approvers,
  how many, the approvals that count and on whose behalf) and its separation of duties.
- `changeSets`: per repository, the change set's hash and the commit it reached, and each change:
  path, operation, the content before and after (base64 when it is not UTF-8), the SHA-256 of the
  content after, and the diff.
- `plans` and `applies`: each stack's latest plan and apply: status, commit, run, times, the
  summary (add, change, destroy), the policy results, the cost, and the resource changes
  (addresses, actions, the names of the attributes that change, never their values).
- `approvals`: every review: who, in which groups, the decision and comment, the change set it
  was for, when, the authentication time, `acr` and step-up, the delegation it used (its ID, the
  delegator, the delegate, the period), and whether it was voided.
- `delivery`: the branch, pull request, head and merge commit, and the merge commit's trailers
  (`Approved-by`, `Break-glass-by`, …), read from the repository's mirror, each email address
  in them replaced by `<address left out>`.
- `breakGlass`: below.
- `events`: the request's audit events as the log holds them (`seq`, `at`, actor, action, object,
  details, `prev`, `hash`), personal data as pseudonyms.
- `personal`: the clear value and the salt of each pseudonym in the request's own events (the
  requester, reviewers, delegators, the breaker, signers, the reason, comments), so the verifier
  makes the pseudonym again; a value pseudonymization erased is not there, and its pseudonym
  stays.
- `proof`: the segments of the log that prove the events, the events nothing proves yet and why,
  and the bound.
- `notes`: what the bundle leaves out, and what its reader must know (an open post-incident
  review, trailers that could not be read).

Left out: IP addresses (even of the request's own events, which keep the pseudonym), email
addresses, plan values and outputs, and secrets of any kind (webhook secrets, tokens, SCIM token
hashes, executor credentials) ([open question 103](../plan/open-questions.md#still-open)).

For a request merged by break-glass (P4-17), the bundle carries the break-glass record in place of
the approvals it lacked: who broke glass, their authentication time, `acr` and step-up, the
incident, the reason, the change set and head it was broken for, the state it was broken from,
whether it lifted a change window, and when; the post-incident review with each sign-off (who, in
which capacity, the comment, when, with what authentication), whether and when it closed; the
`Break-glass-by` and `Break-glass-incident` trailers of the merge commit; and the
`request.break_glass*` audit events with their hashes. A bundle of a broken request whose review
is still open says so, in its notes and on the PDF's first page.

**The proof.** An event's hash covers its content and the event before, and a signed checkpoint
an event's hash, so a checkpoint covers every event up to it. A request's events are scattered
through the log: the bundle proves each by the run of the log from it to the first signed
checkpoint at or after it, whose records, other requests' events included, link its hash to the
checkpoint's. The request's events are grouped by that checkpoint, one segment per group (`from`,
`to`, the checkpoint, and the records of the other events in between; the request's own events are
not repeated). Other events are there as the log holds them, with pseudonyms, never their clear
values. The chain's format does not change: a segment is a slice of what `quai audit export`
writes.

- **Bound.** At most 5,000 records of other events in a bundle (`proof.bound`), the cheapest
  groups first; the events past it are unproven, "beyond the bundle's bound", and the full export
  proves them. Measured on a log of 600 events an hour (a sign-in every six seconds, other
  requests among them), five events in two segments took 1,203 records and 1,166 KiB: about 1 KiB
  a record, so a proof is at most about 5 MB ([open question 101](../plan/open-questions.md#still-open)).
- **Not yet.** An event no signed checkpoint follows is unproven, "no signed checkpoint follows it
  yet": the worker signs every hour. With no checkpoint at all (no signing key,
  `audit.checkpoints.signingKey`), nothing is proven and the bundle says so; Quai never signs with
  a key it made on the fly.
- **Only for those who read the log.** The segments hold other requests' events (their IDs,
  resources, actions, times, commits, states, error texts, and pseudonyms), which only auditors of
  the whole platform and platform admins read: a bundle downloaded in their session carries the
  proof; anyone else's, and any API token's, carries the request's own events and hashes,
  `proof.omitted` saying why ([open question 102](../plan/open-questions.md#still-open)).

**Verifying.** `quai audit verify --bundle bundle.json --key audit-key.pub` checks, offline:

- each event of the request: that it is about the request, in increasing `seq`, its hash the hash
  of its content;
- each clear personal value against its pseudonym, and that an event names it;
- each segment: its range, every `seq` in it once (a missing one is an event removed), none of the
  request's events among its records (one there is an event the bundle's events leave out), each
  record's hash, each `prev` link through the records and the request's events, that it ends at
  its checkpoint, the checkpoint's signature with a key given, and its hash the last record's;
- the change sets: each content's SHA-256 and diff, and the change set's hash computed again from
  its changes;
- the summary against the events where they overlap: the submission (kind, action, type,
  blueprint, repository, definitions commit, reason, requester), the state once final, each
  review (the decision, change set, authentication time, reviewer, comment, delegation, step-up),
  the merge commit, and the break-glass (breaker, reason, incident, change set, head, state,
  authentication time, the sign-offs, whether the review closed, the trailers).

It prints what it verified, and exits with status 1 naming what fails ("event seq 41
(request.reviewed): its hash is not the hash of its content: the event was changed", "segment 1
(seq 40 to 57): seq 49 is missing: an event was removed", "… the events were changed, or the
checkpoint swapped", "the summary's reason … is not the one event seq 40 (request.submitted)
records: the summary was edited"), and when an event is unproven, unless `--allow-unproven`
([open question 105](../plan/open-questions.md#still-open)).

**What it does not prove.** That the bundle holds every event of the request: whoever removes a
whole segment with its events and the parts of the summary they back makes a smaller bundle that
verifies. The export of the whole log, verified with `quai audit verify`, is the completeness
check; the bundle's events are found in it by `seq` and hash. The change set's hash covers each
change's content after, not before: the content before is as Quai read it from the base commit,
and a note says so. A request's state changed by an outside edit (`modified_outside_quai`) has no
audit event: the state is checked only once final.

**Who downloads.** A request's bundle: auditors (of the platform, or of its repository), platform
admins, its requester and its owners (the owner, the members of the owning team); an API token
needs the `evidence:read` scope (`requests:read` is not enough). A sample's list and archive:
auditors of the platform or of the repository, and platform admins. Every download is an audit
event, `evidence.downloaded` ([open question 100](../plan/open-questions.md#still-open)).

**Sampling.** `GET /api/v1/evidence/requests?since=…&until=…` lists the requests created in a
period of at most 366 days, oldest first, by repository, state and break-glass, at most `limit`
(at most 500; `truncated` when more match). `POST /api/v1/evidence/archive` with up to 50 request
IDs streams a ZIP: `request-<number>.json` and `request-<number>.pdf` for each, and
`manifest.json` (the version, when, whether with the proof, each file's request, size and
SHA-256, and the requests left out and why). Names come from the requests' numbers only. At most
256 MiB in all, and at most two archives made at once per server (429 with `Retry-After`)
([open question 106](../plan/open-questions.md#still-open)).

**The PDF summary** is made in Go (fpdf; no browser, no network, no other program): the request,
its rules and reviews, the changes with their diff (cut at 400 lines), the plans and applies, the
delivery and its trailers, the break-glass and its review, the audit events with their hashes (the
first 300), the proof and the notes, and how to verify the JSON. Text from requests is drawn as
plain text, never as links, annotations or scripts; control, format and bidirectional characters
are dropped and each value clipped (2,000 characters, 40 lines). The core fonts draw
Windows-1252: other characters are drawn as `?` ([open question
104](../plan/open-questions.md#still-open)). The JSON is the record.

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
| Segregation of duties (ISO 27001 A.5.3) | No self-approval, distinct approvers, optional definition-author separation and two approvers for destroying changes (`spec.enforcement.separationOfDuties`), break-glass with mandatory review |
| Access control and reviews (A.5.15–A.5.18) | IdP-based authentication, role scopes, role changes in the audit log, SCIM deprovisioning, the access report (every person, their groups, teams, roles by scope and the group that gives each, their live API tokens; a page and a CSV) for periodic reviews ([directory](08-identity-access-tenancy.md#directory)) |
| Logging and monitoring (A.8.15, A.8.16) | Hash-chained audit log, SIEM export (a break-glass, a bypass and lost enforcement as warnings), alerts on bypass and break-glass |
| Emergency changes (ISO 27001 A.8.32, SOC 2 CC8.1, ITIL emergency change) | Break-glass: a dedicated role, a reason and an incident reference, step-up, the content bound, the platform's own protection kept, an immediate alert, and a post-incident review that two reviewers other than the breaker and requester sign off, listed and reminded until it closes |
| Audit evidence (ISO 27001 A.5.28 collection of evidence, SOC 2 CC4.1 and CC8.1 change samples) | An evidence bundle per request, verifiable offline against the signed checkpoints with the public key alone; a period's requests sampled by repository, state and break-glass, their bundles downloaded as one archive with a manifest of their hashes; each download audited |
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
