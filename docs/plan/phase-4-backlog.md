# Phase 4: how autonomous sessions work

Status: Done · 2026-11-17 (the owner closed M2 Beta; Phase 5: [roadmap](roadmap.md#phase-5-blueprints-and-change-sets-5-weeks))

The tasks for [Phase 4](roadmap.md#phase-4-enterprise-governance-7-weeks) are GitHub issues in
milestone [M2 Beta](https://github.com/damhau/quai/milestones) labelled `phase:4` and
`type:task`, titled `P4-NN: …`. `P4-NN` is issue `#(137+NN)`, except P4-00, which is
[#100](https://github.com/damhau/quai/issues/100): the codeowners approvers, formerly P2-01,
postponed until after M1 and kept as its own issue. Their order is their issue number, so #100
comes first. Each names the epic it belongs to (#54–#59), and each epic lists its tasks. GitHub
is the only task list: descriptions and done criteria live in the issues. Read
[`CLAUDE.md`](../../CLAUDE.md) first. An iteration works as in
[Phase 0](phase-0-backlog.md#one-iteration), with `phase:4` instead of `phase:0`:

```bash
gh issue list --repo damhau/quai --label phase:4 --label type:task --state open \
  --json number,title,labels --jq 'sort_by(.number) | .[0]'
```

Commit as `P4-NN: <summary> (closes #<issue>)`. A task may take a few commits; the last one
closes it.

Phase 4 started on 2026-11-08, while P3-19 (#133) waits on the owner to complete !407 to !410;
the [Phase 3 review](phase-3-backlog.md#phase-3-review-p3-21), P3-21 (#135), was written on
2026-11-09 with P3-19 still waiting.

Tests run in [three tiers](../engineering/testing.md#tiers), and an iteration waits only for
the first:
- **Before taking a task,** read the previous push's CI (tier 2) and the last nightly runs
  (tier 3: `e2e.yml`, `kind.yml`, `packaging.yml`, `live.yml`). A red one is fixed first, in a
  commit of its own (`Fix: <summary> (<workflow>)`).
- **Before committing,** run tier 1: `make quick` in `backend/`, and the frontend's checks when
  `frontend/` changed. Run the Playwright scenarios locally only when the task changes them or
  the request flows they cover, and the live tests only when it changes a connector, the runs, or
  what Quai publishes on a pull request. A task that changes `e2e/` dispatches the workflow
  (`gh workflow run e2e.yml`) and waits for it before closing.
- **After committing,** push and take the next task without waiting for CI.
- **The Phase 4 review** (P4-23) is written once tiers 2 and 3 are green on the same commit: it
  dispatches the nightly workflows and waits for them.

The order follows the dependencies:
- **Codeowners** (P4-00, #100): the approver kind the policy already has, fed from the
  connectors' owners. It needs nothing new, and the policy context it fills is the one the
  later approver kinds fill.
- **The audit log:** the chain, checkpoints and `quai audit verify`, the SIEM export (P4-01 to
  P4-03). It comes first so that every feature after it writes its events into the chain from
  its first commit.
- **The directory and catalog entitlements** (P4-04, P4-05): who people are, their emails, teams
  and grants, which notifications, delegation and separation of duties read.
- **Step-up** (P4-06): before anything that approves from outside the request page or breaks
  glass.
- **Notifications:** email, preferences and digests, Microsoft Teams, Slack (P4-07 to P4-10).
- **The other approval features:** manager approvers, separation of duties, delegation,
  reminders, escalation and expiry (P4-11 to P4-14), then SCIM provisioning (P4-15), whose
  deactivations drop approvals and notify again.
- **Change control:** change windows and freezes, break-glass (P4-16, P4-17).
- **Evidence bundles** (P4-18), then **ServiceNow** (P4-19, P4-20), which attaches them to its
  change requests and checks break-glass incident references.
- **The end-to-end scenarios, the M2 rehearsal on `pilot-infra` and the review** (P4-21 to
  P4-23).

## Test infrastructure

Everything of [Phase 3](phase-3-backlog.md#test-infrastructure) stays: embedded PostgreSQL,
Gitea and the Kubernetes API from their binaries, kind for the chart, recorded Terraform plans,
the compose stack's fake runner, the Azure DevOps and GitHub sandboxes, and the stand-in pilot
repository `pilot-infra`. Phase 4 adds:

| What | How |
| --- | --- |
| Services with no account | There are no accounts for ServiceNow, Microsoft Teams, Slack or Entra ID step-up. They are tested on **fakes built from the vendors' documented APIs**, as `TestOIDCLogin` fakes the identity provider ([the owner's decision of 2026-11-07](open-questions.md#decided)). The same goes, for the same reason, for Microsoft Graph (the manager, group overage), Entra ID's SCIM provisioning and Splunk HEC. Each fake is an `httptest` server in a test package of its own (`internal/servicenowtest`, `internal/teamstest`, `internal/slacktest`, `internal/graphtest`, `internal/splunktest`), implementing only the endpoints Quai calls: the documented paths, request and response bodies, authentication, state model and errors (an expired token, throttling with `Retry-After`), keeping what it received for the test to check. SCIM goes the other way, Entra ID calling Quai, so it has no fake server: its tests send Quai the requests Microsoft documents for its provisioning service (`TestSCIMEntraSequence`). Each fake's package comment names the documentation pages it follows and the date they were read. The Phase 4 review lists each as not verified against the real service; real accounts can be added later as live tests without changing the design |
| The identity provider | The fake of `TestOIDCLogin` moves to `internal/oidctest`, for the API tests too, and implements `max_age`, `acr_values`, Entra ID's authentication contexts (the `claims` request parameter, answered with an `acrs` claim), `auth_time`, `oid` and a manager claim (P4-06, P4-11) |
| Mail | [Mailpit](https://github.com/axllent/mailpit)'s release binary, checksum-verified, downloaded once and started by the tests like Gitea's (`internal/mailtest`), its API reading what was sent; and a `mailpit` service in the compose stack (SMTP inside the network, its web page on `:8025`, `MAILPIT_PORT`), which the Playwright scenarios read the same way (P4-07, P4-21) |
| SIEM receivers | In process, in the tests: a syslog listener over TCP and TLS whose messages are checked with an RFC 5424 parser, an OTLP/HTTP logs receiver decoding OpenTelemetry's protobufs, and the Splunk HEC fake (P4-03) |
| Database roles | `pgtest` creates the schema owner and an application role, so tests show that the application role cannot change or delete audit events ([open question 19](open-questions.md#still-open)); the compose stack and the chart connect with both (P4-01) |
| Time | Change windows, expiry, reminders, digests and checkpoints depend on the time. The engine takes the time as an argument (it has no clock), and the services and jobs read a clock that tests set; no test waits for a window to open |
| Keys | The checkpoint signing key (Ed25519) and the chat links' signing key are generated by the tests and passed as `env:` references; never committed |
| The sandboxes | The Azure DevOps and GitHub sandboxes and `pilot-infra`, with their rules: live tests for what Quai publishes on a pull request (an expired request's pull request closed with its comment, a break-glass `quai/approval`), and the M2 rehearsal (P4-22) |

## The M2 rehearsal (P4-22)

The [Phase 4 exit (M2 Beta)](roadmap.md#phase-4-enterprise-governance-7-weeks): *an auditor can
sample ten changes and get, for each, the reason, diff, plan, approvers with authentication
times, merge commit and apply result from one bundle; the audit chain verifies; break-glass works
end to end with alerting.* Rehearsed twice, with the same ten changes:
- **On Gitea, on every push:** `TestAuditSample` (`internal/api/audit_sample_test.go`), on the
  example repository's `vm` type, with plans and applies reported as a runner would (the
  production stack's plan is the recorded one).
- **On the stand-in `pilot-infra`, once:** `TestPilotInfraRehearsal`
  (`internal/api/rehearsal_live_test.go`), run on demand only
  ([open question 124](open-questions.md#still-open)). It ran on 2026-11-09 from 19:34 to 19:46
  UTC (12 minutes), on `main` at `f6d61b8` with this task's tests. Quai was connected to
  `pilot-infra` as P2-13 onboarded it: `connector: azure-devops`, pull requests merged by Quai
  (squash) once approved, `runner.pipelines` naming `pilot-infra-plan` (46) and
  `pilot-infra-apply` (45) for `stacks/{env}`, and the repository's own policies (68 to 70) and
  `vm` type. Nothing of `pilot-infra`'s was changed for it: no policy, pipeline or definition.

**The ten changes on `pilot-infra`.** VMs only: the namespace, DNS and firewall types wait on
!407 to !410, still open ([open question 16](open-questions.md#still-open)). Each change was
planned, decided, merged and applied before the next one was submitted. Each plan is the pull
request's build validation run, which Quai read when it polled. The test posted, as the run's
`quai report` step would, Terraform's plan of each stack (`terraform show -json`), made as the
pipeline makes it: on the pull request's head, with the state the last apply kept. Each apply
is the apply pipeline's run at the merge commit, which Quai found when it polled, with nothing
reaching Quai.

| # | Change | Decided by | Pull request | Plan run | Merge commit | Apply run |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | bob creates `prod/m2-web` | alice (production) | !1341 | 517: +1 | `306ae66c1696` | 518 |
| 2 | carol creates `prod/m2-db` | pat (production) | !1342 | 519: +1 | `f28a8638f472` | 520 |
| 3 | bob creates `dev/m2-ci`, 8 vCPUs | fiona (large VM) | !1343 | 521: +1 | `810ec46c8121` | 522 |
| 4 | bob resizes `prod/m2-web` | alice | !1344 | 523: ~1 | `c0850af5f4fb` | 524 |
| 5 | carol resizes `prod/m2-db` to 8 vCPUs | pat and fiona (production, large VM) | !1345 | 525: ~1 | `f7922d5b7ecd` | 526 |
| 6 | bob creates `prod/m2-tmp` | denied by alice | !1346, closed by Quai | 527: +1 | none | none |
| 7 | bob resizes `prod/m2-web` to 8 vCPUs | dave breaks glass, INC-2204 | !1347 | 528: ~1 | `4dc0f56665d4` | 529 |
| 8 | bob deletes `prod/m2-web` | alice | !1348 | 530: -1 | `528b6080cf2a` | 531 |
| 9 | carol deletes `prod/m2-db` | pat | !1349 | 532: -1 | `108a727409d0` | 533 |
| 10 | alice deletes `dev/m2-ci` | bob, its owner | !1350 | 534: -1 | `a1498d634045` | 535 |

The VMs' names end in `-11091934`, the run's tag. Every VM the rehearsal created it deleted, so
`main`'s tree at `a1498d6` is the tree it had before, at `88bf1ca`. No `quai/` branch was left
over, no pull request is open but !407 to !410, which nothing touched, and nothing else was
created in the sandbox.

**What the auditor got.** Erin, an auditor of everything, listed the period's requests (all ten,
the one broken, the one rejected) and downloaded their bundles as one archive (the JSON bundles,
the PDF summaries and the manifest of their hashes). Each bundle holds:
- the reason;
- the diff;
- both stacks' plans, Terraform's, the request's VM the one resource they change, as the
  request does;
- the approvers, each with their authentication time (the denial's too);
- for the break-glass, the breaker with their authentication time, the incident, and the
  post-incident review, closed by erin's and alice's sign-offs;
- the pull request, its merge commit and trailers;
- both stacks' applies at that commit.

**What verified.**
- **The bundles:** `quai audit verify --bundle` on each of the ten, with the checkpoint's public
  key: every event of each request proven by checkpoint seq 77 (3 to 7 events a bundle).
- **The export:** `quai audit verify` on the audit log's export: 78 events from seq 1 to 78,
  2 checkpoints matching.
- **The alert:** the break-glass alert reached the alert address in a local Mailpit, with the
  incident and the request's link, without the reason.

`TestAuditSample` checks the same on Gitea.

**Not verified.**
- **The other types:** namespaces and quotas, DNS records and firewall rules on `pilot-infra`,
  until !407 to !410 are completed (open question 16). On Gitea, `TestPilotTypes` and
  `TestPilotConcurrentEntries` request them through pull requests, without bundles.
- **The pipelines' own `quai report` step:** the agents cannot reach a local Quai. The test posted
  the plan the step would post, made by Terraform the same way, and Quai learned the applies by
  polling. The service hooks were not used either.
- **The identity:** sign-ins are the harness's dev users, so the authentication times are their
  sessions'. Entra ID's `auth_time` and step-up on approvals are tested on the fake identity
  provider only (P4-06).
- **The mail:** a local Mailpit, never a real mailbox.
- **The checkpoints:** signed by the test with a key it generated. The keeper (checkpoints
  committed to an audit repository) and `quai audit verify --anchors` were not part of the run;
  `TestAuditExportVerify` covers them.
- **Not configured:** ServiceNow, Teams and Slack, delegation, change windows and separation of
  duties. They have their own tests, on fakes for the services without an account.
- **The Quai name:** Quai named the connected repository `infra-live`, the harness's name, so the
  commit messages say `infra-live/vm/…` where P2-13's said `pilot-infra/vm/…`.
- **The platform:** Azure DevOps Services. The pilot's own platform, Azure DevOps Server, is a
  [future improvement](roadmap.md#future-improvements).

## Phase 4 review (P4-23)

2026-11-09, with every Phase 4 task closed. The
[Phase 4 exit](roadmap.md#phase-4-enterprise-governance-7-weeks), and M2's governance
([overview](roadmap.md#overview): roles, separation of duties, change windows, break-glass, ITSM,
tamper-evident audit), against the tests that show them:
- **On every push:** the backend tests, on Gitea and with the fakes of the services with no
  account ([test infrastructure](#test-infrastructure)), and the frontend's.
- **On the Azure DevOps Services and GitHub sandboxes, nightly:** the live tests (`live.yml`).
- **On the compose stack, nightly:** the Playwright scenarios, with governance since P4-21
  (`e2e/tests/governance.spec.ts`, `e2e/tests/notifications.spec.ts`).
- **On the stand-in `pilot-infra`, once:** the [M2 rehearsal](#the-m2-rehearsal-p4-22), on Azure
  DevOps Services.

Tiers 2 and 3 were green on `ece1b9b` (2026-11-09, about 20:40 UTC): on the push, `backend`
(run 42692815270), `frontend` (42692815461), `packaging` (42692815464) and `plugins`
(42692815253); dispatched, `kind` (42692812971), `e2e` (42692818394) and `live` (42692825265),
whose GitHub job passed 183 tests and subtests and skipped one,
`TestLiveConformance/4-status-identity`, and whose Azure DevOps job passed. The commits after it
change CI and docs only: `3671bba` adds the editors' fuzz jobs, its push CI green with them
(`backend` 42694702148), and `c99d2e2` is the Phase 3 review. A push does not run the image's
vulnerability scan (tier 3), so `packaging` was dispatched on `c99d2e2`, whose image is
`ece1b9b`'s: green (42696548658), Trivy finding no fixed HIGH or CRITICAL vulnerability.

**After the review**, until the owner closed M2 on 2026-11-17:
- **Fixes:**
  - `b02d512`: the HCL editor took the lines of a removed object or tuple entry with whatever
    shared them (another entry, or the closing bracket). Such a removal is now refused. The
    nightly fuzz job found it.
  - `dc35a59`: fuzzing is removed, the targets and the jobs with it (the owner's decision of
    2026-11-14).
  - `87fc72a`: the designer published a hand-written list or object form field as a string. It
    now keeps such a field as written ([design 04](../design/04-blueprints-and-change-sets.md)).
  - `8d1d2c4`: Go 1.27.4, for GO-2026-6859 (an HTTP/1 server crash in `net/http`, which
    `govulncheck` found reachable from the API).
  - `b7d100d`: `TestLiveForks` asks again when GitHub answers 500 to a branch in a fork it has
    just made (live, 2026-11-15).
- **Seen and not changed:** `TestLiveOwners` read `CODEOWNERS` back empty on 2026-11-12, and has
  passed every night since.
- **Tiers 2 and 3 green on `b7d100d`:** on the push (2026-11-15), `backend` (43745720585),
  `frontend` (43745720553), `packaging` (43745720529) and `plugins` (43745720600); nightly on
  2026-11-16 and 2026-11-17, `kind` (43995023189), `e2e` (43998360709), `packaging` with Trivy's
  scan (43994318453) and `live` (44003201373), whose GitHub job passed 183 tests and subtests and
  skipped `TestLiveConformance/4-status-identity` again, and whose Azure DevOps job passed.

**The exit, part by part.**

| Exit | Every push (Gitea) | Azure DevOps Services and GitHub | Compose stack | `pilot-infra` |
| --- | --- | --- | --- | --- |
| An auditor samples ten changes and gets, for each, the reason, diff, plan, approvers with authentication times, merge commit and apply result from one bundle | `TestAuditSample`: ten changes of the example's `vm` type, one denied and one broken glass, sampled by erin and downloaded as one archive. `TestEvidenceBundles`, `TestEvidenceSample`, `TestEvidenceTrailersWithoutAddresses`; `internal/evidence`'s `TestBundleVerifies` and `TestPDF`; the frontend's `evidence.test.tsx` | Azure DevOps Services: `TestPilotInfraRehearsal` (last column). GitHub: none | "an auditor reads a request's audit events, samples it and checks its evidence bundle" | `TestPilotInfraRehearsal`: !1341 to !1350, ten bundles in one archive |
| The audit chain verifies | `internal/audit`'s `TestCanonical`, `TestConcurrentWriters` and `TestExportVerifies` (each tampering fails at its `seq`); the keeper's `TestCheckpoints` (checkpoints signed and committed to an audit repository, the daily verification, a database restored from an older backup found), `TestRotation`, `TestAuditRepository`; `cmd/quai`'s `TestAuditExportVerify` and `TestAuditVerifyBundle`; `internal/evidence`'s `TestEditedBundleFails`; `TestAuditSample` | Azure DevOps Services: `TestPilotInfraRehearsal`. GitHub: none | The auditor scenario: the stack has no signing key, so its bundle's events are unproven, as the scenario checks | `quai audit verify` on the export (78 events, 2 checkpoints) and `--bundle` on each of the ten |
| Break-glass works end to end with alerting | `TestBreakGlass` (who may break, the merge naming the breaker and the incident, the alert by email with identifiers only, two sign-offs), `TestBreakGlassChangeWindow`, `TestBreakGlassManualMerge`, `TestBreakGlassDirect`, `TestBreakGlassRace`, `TestBreakGlassSlack`; with ServiceNow's fake, `TestServiceNowBreakGlass`, `TestServiceNowIncidents`, `TestServiceNowIncidentUnverified` | `TestAzureDevOpsBreakGlass`, `TestGitHubBreakGlass`: `quai/approval` "Break-glass by dave", and the platform completes the pull request. The alert is Quai's, checked on Gitea | "break-glass merges a pending request for an incident, alerts, and its review closes on two sign-offs", the alert read in Mailpit | Change 7: dave breaks glass for INC-2204, the alert reaches a local Mailpit, erin and alice sign the review off |
| Roles and teams | `internal/authz`'s `TestAuthorize`; `TestEveryHandlerAuthorizes` (every operation authorizes); `TestAuditReaders`, `TestTeams`, `TestUnmappedTeams`, `TestTeamInstances`, `TestAccessReport`, `TestEntitlements`, `TestCodeowners`; SCIM's `TestSCIMGroups`, `TestSCIMDeactivation` | `TestAzureDevOpsCodeowners`, `TestGitHubCodeowners`: the platform's code owners approve and own | The scenarios sign in as alice (platform admin), bob (requester), erin (auditor) and fiona (break-glass, through `oncall`) | Erin the auditor, dave breaking glass, the approvers of the repository's own policies |
| Separation of duties | `TestSeparationDefinitionAuthors`, `TestSeparationRequireTwo`, `TestSeparationDirect`; the policy's `TestDefinitionAuthors`, `TestRequireTwo`, `TestDistinctApprovers`, `TestDelegateRefused`, `TestDelegationCountsOnce`; who may break glass and sign off, in `TestBreakGlass` | None | The delegation scenario: the delegate's approval recorded on the delegator's behalf | Not configured |
| Change windows and freezes | `TestChangeWindowFreeze` (a repository's window does not lift Quai's freeze), `TestChangeWindowAutoComplete`, `TestChangeWindowManualMerge`, `TestChangeWindowDirect`, `TestChangeWindowConditionError`; the engine's `internal/engine/window` tests (`TestAtDST` among them); `TestBreakGlassChangeWindow` | `TestAzureDevOpsChangeWindow`, `TestGitHubChangeWindow` (auto-merge and the merge queue): nothing armed and no merge group built before the window opens | "a freeze on the repository holds an approved request, which merges once the freeze is lifted" | Not configured |
| ITSM | ServiceNow, on its fake: `TestServiceNowNormalChange`, `TestServiceNowStandardChange`, `TestServiceNowRejected`, `TestServiceNowWithdrawn`, `TestServiceNowSuperseded`, `TestServiceNowUnreachable`, `TestServiceNowFailsClosed`, `TestServiceNowClosed`, `TestServiceNowClosedAfterApply`, `TestServiceNowCloseRefused`, `TestServiceNowSupersededNotClosed`; the client's tests in `internal/servicenow` | None: there is no instance | None | Not configured |
| Tamper-evident audit | `TestInsertOnly` (the application role cannot change or delete events), `TestPrune` (by the database's clock, up to a checkpoint), `TestAuditLog`, `TestAuditExport`; `cmd/quai`'s `TestMigrate` (two roles, a warning with one) and `TestWorker`; the SIEM export's `TestSyslog`, `TestOTLP`, `TestSplunk`, `TestLag`, `TestPrunedBeforeExport`, `TestChangedAfterExport`. On kind, nightly: the chart installed with both database roles | None | The auditor scenario reads the request's audit events | The export verified, its checkpoints signed with the test's key; the keeper was not part of the run |

**Also delivered**, on Gitea on every push unless named:
- **Step-up** (P4-06): `TestStepUpApprovals`, `TestStepUpAdmin`, `TestDelegationStepUp`; the
  round trip with Entra ID's authentication context on `internal/oidctest`, `TestStepUpLogin`.
- **Notifications** (P4-07 to P4-10): email through Mailpit (`TestNotifications`,
  `TestAlertNotifications`, the `TestEmail…` tests of `internal/notify`), preferences and digests
  (`TestPreferences`, `TestDigest`), Teams and Slack on their fakes (`TestMicrosoftTeams`,
  `TestSlack`), the chat links (`internal/chatlink`); e2e `notifications.spec.ts`.
- **Manager approvers** (P4-11): `TestManagerFromGraph`, `TestManagerFromClaim`.
- **Delegation** (P4-13): `TestDelegation`, `TestDelegationOnGitea`; the e2e delegation scenario.
- **Reminders, escalation and expiry** (P4-14): `TestApprovalReminders`, `TestApprovalExpiry`;
  live, `TestAzureDevOpsExpiry` and `TestGitHubExpiry`.
- **SCIM provisioning** (P4-15): `TestSCIMEntraSequence`, `TestSCIMIdentityBinding`,
  `TestSCIMToken`.

**Not verified against a real service.** Each is tested against the vendor's documentation only,
as [decided](open-questions.md#decided) on 2026-11-07: on a fake whose package comment names the
pages and the date they were read (2026-11-09), or, for SCIM, with the requests Microsoft
documents. Each can become a live test once there is an account.

| Service | Tested on | First to check against the real service |
| --- | --- | --- |
| ServiceNow | `internal/servicenowtest`: the `TestServiceNow…` tests of `internal/api`, the client's tests in `internal/servicenow` | On a sub-production instance, with the service account's roles the [operations guide](../operations/deployment-and-operations.md#servicenow-change-requests) names: a normal change created through the Change Management API keeping its `correlation_id`, then moved by PATCH to Implement, Review and Closed, which the fake assumes needs no wait for the planned start; then the attachments and an incident's `rfc`. The edges P4-19 and P4-20 left are below |
| Microsoft Teams | `internal/teamstest` (the token endpoint, the Bot Connector, a Workflows webhook): `TestMicrosoftTeams`, `TestMicrosoftTeamsReach`, `TestMicrosoftTeamsStepUp`, the `TestTeams…` tests of `internal/notify` | A single-tenant bot, its app installed by a setup policy: a personal card created by a person's `oid` and updated after their decision, the 403 when the app is not installed, and a channel post through a Workflows webhook |
| Slack | `internal/slacktest`: `TestSlack`, `TestSlackPreferences`, `TestBreakGlassSlack`, the `TestSlack…` tests of `internal/notify` | An app with the documented scopes: `users.lookupByEmail` on a member's verified email (the fake matches without regard to case), a direct message and its `chat.update`, a button's signed interaction at `/slack/interactions` acknowledged within three seconds |
| Entra ID step-up | `internal/oidctest`: `TestStepUpLogin` (the round trip, with an authentication context), `TestStepUpCheck`. The API's step-up tests sign in through the development switcher | A tenant set up as the [operations guide](../operations/deployment-and-operations.md#step-up-authentication) says: the ID token after a step-up holding `acrs` with the context and a fresh `auth_time`; `xms_edov` in it, for emails to count ([open question 30](open-questions.md#still-open)) |
| Microsoft Graph | `internal/graphtest`: `TestManagerFromGraph`, `TestGraphGroups`, `TestGraphManager` | `/me/manager` with `User.Read.All` consented, and `getMemberObjects` for a person over the ID token's group limit, answering every ID at once as the fake assumes |
| Entra ID SCIM provisioning | No fake server, Entra ID calling Quai: `TestSCIMEntraSequence` sends the requests Microsoft documents; `TestSCIMGroups`, `TestSCIMDeactivation`, `TestSCIMIdentityBinding`, `internal/scim`'s tests | An enterprise application provisioning to `/scim/v2`: its test connection, a user created with `externalId` mapped to the object ID ([open question 84](open-questions.md#still-open)), then disabled and deleted, read back in the provisioning logs |
| Splunk HEC | `internal/splunktest`: `TestSplunk` | A token with indexer acknowledgment off ([open question 41](open-questions.md#still-open)): a batch taken with `200` and searchable with its `time`, `host`, `source` and `sourcetype`; a refused batch, which the fake assumes takes none of its events |

Also not verified: syslog with rsyslog or syslog-ng (closing after a half-close is assumed), an
OpenTelemetry collector, the chart's `PrometheusRule` in a running Prometheus; Entra ID's `oid`
claim, which only the tests set; checkpoints on the compose stack or kind, which have no signing
key (the worker warns, [open question 38](open-questions.md#still-open)). The e2e scenarios do not
cover chat, ServiceNow, step-up, or the evidence PDF and archive.

**Open questions whose defaults the owner should confirm.** Phase 4 recorded 17 to 124, all
still open ([open questions](open-questions.md#still-open)): 17 to 28 with the Phase 4 plan, the
rest in the tasks. 13 to 16 are the [Phase 3 review's](phase-3-backlog.md#phase-3-review-p3-21)
(item 5). Two name M2 itself: 56 (needed by Beta) and 124 (needed by this review).

Security-sensitive or hard to reverse:
- **18:** an event's hash is SHA-256 over RFC 8785's form, personal values salted and hashed. It
  cannot change once a chain exists.
- **19, 36, 37:** two database roles, and pruning by the database's clock and retention, only up
  to a checkpoint. They keep a compromised application from changing or deleting the log; 36 was
  fixed in review (557cf61).
- **34, 35, 38:** by default the pods can read the schema owner's Secret (the chart warns), the
  server can read the checkpoint key, and the worker runs without a key in production (it warns).
- **20:** once `auth.teams` is set, a group it does not map is no team (f624992). It decides who
  owns and approves.
- **30, 45:** an email counts only when the IdP vouches for it (`xms_edov` with Entra ID), and a
  name the directory holds for another account refuses the sign-in.
- **49 to 51:** step-up fails closed (five minutes by default; no `auth_time`, no step-up), for
  the whole request, denials included, and for every repository change.
- **22, 61, 62, 67:** a chat link never approves on its own, signed with a key of its own and
  valid a day; Slack's endpoint only verifies and acknowledges.
- **83 to 86, 88:** SCIM's token, people bound by an `externalId` that never changes once set,
  groups only taken away, deactivation at once, the reserved break-glass accounts.
- **27, 94 to 99:** who may break glass (a role platform admin does not imply, in a browser
  session), what it lifts (Quai's approvals and windows only) and who signs its review off.
- **121:** break-glass goes ahead when ServiceNow cannot say whether the incident exists, checked
  again later.
- **39, 40, 44, 100, 103:** personal data. The audit log's is erased by the schema's owner by
  hand, a SIEM gets pseudonyms only, directory rows are never erased, and a bundle carries its
  request's people in clear: once downloaded, it cannot be erased.

By task:
- **P4-00:** 17 a GitHub `@user` in `CODEOWNERS`, by the mapping then a verified-domain email;
  29 the mapping's form, owners kept at submission; 30 vouched-for emails only.
- **P4-01:** 18 the hash's serialization; 19 two roles; 31 where an unprotected log is said;
  32 the peer address, not `X-Forwarded-For`; 33 a run token's revocation not recorded; 34 the
  chart and the owner's Secret.
- **P4-02:** 35 old keys in configuration, the public key not served; 36 the database's clock
  prunes; 37 what pruning checks of a checkpoint; 38 no key in production warns; 39 pseudonymizing
  by hand.
- **P4-03:** 40 pseudonyms to SIEMs; 41 no HEC acknowledgment; 42 plaintext to a loopback address
  only; 43 pruning does not wait for the export.
- **P4-04:** 20 `auth.teams`; 44 directory rows not erased; 45 a reused name refused; 46 tokens
  in the access report, acting with their creation groups.
- **P4-05:** 21 `spec.entitlements`; 47 a blueprint needs its types' entitlements; 48
  entitlements govern creating only.
- **P4-06:** 49 what a recent authentication is; 50 the whole request steps up; 51
  `auth.stepUp.admin` covers every repository change.
- **P4-07:** 52 email never without TLS; 53 no start without `server.publicURL`; 54 less of the
  plan than the page; 55 alerts' recipients, identifiers only; 56 recipients decided at fan-out;
  57 twenty attempts, at least once.
- **P4-08:** 58 preferences in a session only, audited; 59 the defaults, one digest a day; 60 who
  counts as reached.
- **P4-09:** 22 chat links; 61 their signing, lifetime and rotation; 62 a link's `GET` changes
  nothing, refusals audited; 63 a single-tenant bot, to the `oid`; 64 channel posts with
  identifiers only; 65 when a card is updated.
- **P4-10:** 66 a Slack message by verified email; 67 the interactions endpoint.
- **P4-11:** 68 Graph read with the sign-in's own token; 69 the manager by an immutable
  identifier; 70 kept at submission.
- **P4-12:** 23 separation of duties in the repository document; 71 definition authors by
  verified email; 72 the period by what Quai recorded; 73 `requireTwo`; 74 a stricter setting
  makes pending requests stale.
- **P4-13:** 24 what a delegate may do; 75 the delegator in a session, nothing passed on; 76 one
  human counts once; 77 only "approval needed" goes to the delegate.
- **P4-14:** 25 reminders and escalation in Quai's configuration, escalation only notifying; 78
  the expiry as submitted; 79 each once from the submission; 80 expired is final; 81 the pull
  request Quai opened closed with one comment; 82 what an escalation tells whom.
- **P4-15:** 83 the SCIM token; 84 binding by `externalId`; 85 SCIM only takes groups away; 86 a
  deactivation; 87 the manager from SCIM; 88 reserved names, how much of SCIM is implemented.
- **P4-16:** 26 windows in the configuration and the repository document; 89 every window must
  be open; 90 the syntax and zones; 91 what `when` sees; 92 an approved request waiting does not
  expire; 93 a window closing on a merge in flight.
- **P4-17:** 27 who signs off, who "security" is; 94 who may break glass; 95 which requests; 96
  what it lifts; 97 the reason and incident; 98 whom it alerts; 99 the review.
- **P4-18:** 100 who downloads a bundle; 101 the proof by segments; 102 what the proof discloses,
  the ITSM attachment without it; 103 what is in clear; 104 the PDF's fonts; 105 when `--bundle`
  passes; 106 a sample's filters and bounds.
- **P4-19:** 28 ServiceNow's standard changes, polling, authentication; 107 one change request per
  content; 108 the sys_id and correlation ID; 109 which states approve; 110 the fields written;
  111 polling, ServiceNow unreachable; 112 break-glass lifts `external: servicenow`; 113 type and
  blueprint rules only; 114 a change request left as it is.
- **P4-20:** 115 closing on apply; 116 the attachments; 117 a superseded change request; 118 one
  that cannot be closed the normal way; 119 the incident's `rfc`; 120 the incident states; 121 an
  incident ServiceNow cannot confirm; 122 incident numbers only.
- **P4-21:** 123 the compose stack's repository connected through the API.
- **P4-22:** 124 the rehearsal run on demand.

**Moved out of Phase 4, or still open.**
- **P3-19 on `pilot-infra`** ([open question 16](open-questions.md#still-open)): the rehearsal
  used the `vm` type only, as !407 to !410 waited for the owner's policy bypass
  ([Phase 3 review](phase-3-backlog.md#phase-3-review-p3-21), item 1). The owner completed them on
  2026-11-15 (apply run 626 passed) and closed #133 on 2026-11-17. The namespace, DNS and firewall
  types are requested through Quai on Gitea (`TestPilotTypes`, `TestPilotConcurrentEntries`), not
  on `pilot-infra` itself.
- **Quai reviewing pull requests it did not open**, the alternative to open question 16's
  default: in no phase, as no design document specifies it ([STOP tasks](#stop-tasks)).
- **Outgoing webhooks**, and with them the audit export and the `external` approver over a
  webhook: Phase 7 ([#70](https://github.com/damhau/quai/issues/70)).
- **Jira Service Management:** when a customer needs it
  ([open question 4](open-questions.md#decided)).
- **CEF:** optional in #140, not built
  ([design 09](../design/09-security-and-compliance.md#export-to-a-siem)); the epic #57 still
  says "syslog/CEF".
- **Azure DevOps Server and GitHub Enterprise Server:**
  [future improvements](roadmap.md#future-improvements) (#98, #137). M2 was rehearsed on Azure
  DevOps Services.
- **The options the open questions leave unbuilt**, such as `quai users forget` (44), trusted
  proxies (32), checkpoints that are Merkle trees (101) or HEC's acknowledgment (41): for the owner
  to schedule with the answers.

**What the review found.**
- **Security fixes, found in review**, each a commit of its task:
  - `557cf61` (P4-02): pruning aged events by the `at` the application writes, so a compromised
    application could date events back and prune the whole log. Events now age by
    `audit_events.recorded`, the database's clock at insertion, which the application role cannot
    write ([open question 36](open-questions.md#still-open)).
  - `f624992` (P4-04): with `auth.teams` set, a group no entry matched was still the team of its
    name, so another department's group sharing a team's name made its members that team's owners
    and approvers. It is now no team ([open question 20](open-questions.md#still-open)).
  - `d7fd25a` (P4-04, found with P4-05): instance reads checked the raw owner, refusing team
    members their team's instances; those are now read through their team.
- **Flakes fixed**, each in a `Fix:` commit, the product unchanged: a Mailpit taking another's
  port in `internal/mailtest` (`fa03412`), `TestChangeWindowAutoComplete` waiting for a state that
  lasts an instant (`711ae24`), the frontend's scoped-auditor evidence test reading the select
  before its options (`694d33a`), `TestSandbox`'s memory case reaching the time limit first
  (`f6d61b8`), and Mailpit downloaded on every run, which failed during GitHub's incident, now
  cached (`48f6a54`). Seen and not changed, each passing alone or on a re-run:
  `TestResourceStatus`, `internal/secrets`' envtest start under load, `TestNewPlanNeedsApproval`,
  `TestWorker` and the keeper's tests with the runner's PostgreSQL dropping connections, the e2e
  `shared-files` and `runs` scenarios' timeouts, `TestLiveAutoMerge` waiting on GitHub's
  mergeability, and one submit answered 500 in `TestAzureDevOpsExpiry` (`a809667` prints the
  server's log for the next).
- **Edges noted, not changed:**
  - **The delegate's view** (P4-21): a rule undecided until the plan (a type's cost,
    `group:finops`) counts its approvers as awaited, so a delegate who is also one of them gets no
    "on behalf" sentence and is offered "Review as: Yourself" once the plan rules the rule out.
    The counting is the policy's, so no approval is miscounted.
  - **The rehearsal's Terraform** (P4-22, [open question 124](open-questions.md#still-open)):
    `TestPilotInfraRehearsal` runs `terraform init` and `plan` on the tester's machine, on
    `pilot-infra`'s files at each pull request's head. The stand-in holds `terraform_data` only; a
    provider or provisioner in a real pilot's repository would run there too.
  - **Hidden catalog items** (P4-05): a `catalog: hidden` type can still be requested through the
    API (hidden is a listing flag, entitlements are the access control), and a hidden item
    answers 403 rather than 404, so signed-in callers learn it exists.
  - **Item pages** (P4-05): `get-type` and `get-blueprint` check the catalog's read role before
    `?resource=` or `?instance=`, so someone who only owns or operates a resource opens it but not
    its item's page. A blueprint's card shows only its own lists; its types' refusals show at the
    plan ([open question 47](open-questions.md#still-open)).
  - **Definition authors** (P4-12, [open question 71](open-questions.md#still-open)): matched by
    verified email on a commit's author, committer and co-authors only. No connector exposes who
    pushed, opened or merged, so an author committing under an unknown email is not excluded;
    `CODEOWNERS` on `.quai/` remains the control.
  - **An expired request's comment** (P4-14, [open question 81](open-questions.md#still-open)):
    someone who first comments `<!-- quai:expired=<id> -->` on the pull request suppresses Quai's
    comment; the status and the close still happen. Matching only Quai's own comments would close
    it.
  - **A window closing on a merge in flight** (P4-16,
    [open question 93](open-questions.md#still-open)): a merge group, or an armed auto-merge whose
    checks pass within the sweep's minute, can still land; with `merge: manual`, a
    `quai/approval` success stays until the first sweep after the window closes.
  - **ServiceNow's answers** (P4-19, P4-20): a create that lists `correlation_id` in
    `__meta.ignoredFields` yet echoes the ID records a problem, but a later poll with a matching
    ID would take its approval; an incident query whose results hold no exact match counts as
    unclear, so break-glass goes ahead unverified and a job checks again. Either answer would be
    an anomaly of ServiceNow's.
  - **Evidence across repositories** (P4-18): a request with change sets in several repositories
    is listed for an auditor of any of them but authorized on its first: stricter, not a leak.
  - **The audit log** (P4-02): the application role can append an `audit.verified` event, which
    hides the page's banner for about a day (the metric and the worker's log are separate), and
    verification does not flag an `at` going backwards.
  - **SCIM** (P4-15): one rate limit is shared by every SCIM token, and a deactivated person's
    chat links rely on opening a link needing a session.
  - **The navigation** (P4-18): a platform admin's labels wrap at 1900 px.
  - **The test harnesses:** the free-port race fixed in `internal/mailtest` may exist in
    `giteatest`, `pgtest` and `k8stest` (not checked).
- **GitHub's failures of 2026-11-09** are in the Phase 3 review's item 7, with the retries each
  got. The last two came after P4-22, in the GitHub connector's live tests, the connector
  unchanged: a status read back as it was before it was set (`TestLiveRuleset` on `8fc2a48`,
  fixed in `7332cfd`), and a merge queue enqueue answered 502 (`TestLiveMergeQueue` on `7332cfd`,
  fixed in `ece1b9b`).
- **Azure DevOps' maintenance window.** On `9c507e0` (P4-14, about 10:44 UTC), Azure DevOps
  Services answered "Service maintenance is in effect": `TestAzureDevOpsDelivery`'s pull request
  job timed out listing pull requests, and the test's cleanup timed out deleting
  `quai-test-24012989`, which is still in the sandbox. The next run, on `8c7d9ae`, passed.

**What waits for the owner.**
1. **The leftover sandbox repositories**, to delete: on GitHub `quai-test-fb683e3d` and
   `quai-test-31c3b3d3` (2026-11-08), on Azure DevOps `quai-test-24012989` (2026-11-09). All
   three are still there on 2026-11-17.
2. **The GitHub sandbox's webhook:** a URL GitHub can reach
   ([Phase 3 review](phase-3-backlog.md#phase-3-review-p3-21), item 2).
3. **The open questions** above, the security-sensitive ones first.
4. **Entra ID, when there is a tenant:** the optional claims `auth_time`, `acrs` and `xms_edov` in
   the ID token, and an authentication context with its Conditional Access policy
   ([step-up](../operations/deployment-and-operations.md#step-up-authentication),
   [people and teams](../operations/deployment-and-operations.md#people-and-teams)).
5. **Optionally, a second GitHub App** on the sandbox organization, for conformance step 4
   ([Phase 3 review](phase-3-backlog.md#phase-3-review-p3-21), item 4).
6. **The Azure DevOps PAT's expiry:** the `AZURE_DEVOPS_PAT` secret of `live.yml`, and the local
   copy the rehearsal reads.
7. **ServiceNow and the other faked services**, checked against real instances as above.

## STOP tasks

- **P4-23** (Phase 4 review): write the report the issue describes, commit with `refs`, comment
  that it is ready, and **stop**. Only the owner closes it, and M2.

No other task needs the owner: the services with no account are tested on fakes (decided), and
the M2 rehearsal uses `pilot-infra` as it is. Its namespace, DNS and firewall types wait on
P3-19's pull requests !407 to !410, which only the owner can complete
([open question 16](open-questions.md#still-open)); if they are still open, the rehearsal uses
the VM type and says so.

Open question 16 still waits for the owner: its default, an owner's policy bypass for people's
own pull requests, stands. Quai reviewing pull requests it did not open is not a Phase 4 task:
no design document specifies it. It becomes one if the owner decides so.

Open questions 17 to 28 ([open questions](open-questions.md#still-open)) have defaults the tasks
follow until the owner answers them. Where a default is security-sensitive, it is the stricter
reading of the design: a chat link never approves on its own (22), escalation never adds
approvers (25), types cannot set their own change windows or separation of duties (23, 26).

The rules of [when to stop and ask](phase-0-backlog.md#when-to-stop-and-ask) apply.
Left out of Phase 4: outgoing webhooks, and with them the audit export and the `external`
approver over a webhook, are Phase 7 ([#70](https://github.com/damhau/quai/issues/70)); Jira
Service Management comes when a customer needs it
([open question 4](open-questions.md#decided)).
