# Phase 4: how autonomous sessions work

Status: Active · 2026-11-08

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
the Phase 3 review, P3-21 (#135), is written once P3-19 is done.

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
| Services with no account | There are no accounts for ServiceNow, Microsoft Teams, Slack or Entra ID step-up. They are tested on **fakes built from the vendors' documented APIs**, as `TestOIDCLogin` fakes the identity provider ([the owner's decision of 2026-11-07](open-questions.md#decided)). The same goes, for the same reason, for Microsoft Graph (the manager, group overage), Entra ID's SCIM provisioning and Splunk HEC. Each fake is an `httptest` server in a test package of its own (`internal/servicenowtest`, `internal/teamstest`, `internal/slacktest`, `internal/graphtest`, `internal/splunktest`), implementing only the endpoints Quai calls: the documented paths, request and response bodies, authentication, state model and errors (an expired token, throttling with `Retry-After`), keeping what it received for the test to check. Each fake's package comment names the documentation pages it follows and the date they were read. The Phase 4 review lists each as not verified against the real service; real accounts can be added later as live tests without changing the design |
| The identity provider | The fake of `TestOIDCLogin` moves to `internal/oidctest`, for the API tests too, and implements `max_age`, `acr_values`, Entra ID's authentication contexts (the `claims` request parameter, answered with an `acrs` claim), `auth_time`, `oid` and a manager claim (P4-06, P4-11) |
| Mail | [Mailpit](https://github.com/axllent/mailpit)'s release binary, checksum-verified, downloaded once and started by the tests like Gitea's (`internal/mailtest`), its API reading what was sent; and a `mailpit` service in the compose stack (SMTP inside the network, its web page on `:8025`, `MAILPIT_PORT`), which the Playwright scenarios read the same way (P4-07, P4-21) |
| SIEM receivers | In process, in the tests: a syslog listener over TCP and TLS whose messages are checked with an RFC 5424 parser, an OTLP/HTTP logs receiver decoding OpenTelemetry's protobufs, and the Splunk HEC fake (P4-03) |
| Database roles | `pgtest` creates the schema owner and an application role, so tests show that the application role cannot change or delete audit events ([open question 19](open-questions.md#still-open)); the compose stack and the chart connect with both (P4-01) |
| Time | Change windows, expiry, reminders, digests and checkpoints depend on the time. The engine takes the time as an argument (it has no clock), and the services and jobs read a clock that tests set; no test waits for a window to open |
| Keys | The checkpoint signing key (Ed25519) and the chat links' signing key are generated by the tests and passed as `env:` references; never committed |
| The sandboxes | The Azure DevOps and GitHub sandboxes and `pilot-infra`, with their rules: live tests for what Quai publishes on a pull request (an expired request's pull request closed with its comment, a break-glass `quai/approval`), and the M2 rehearsal (P4-22) |

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
