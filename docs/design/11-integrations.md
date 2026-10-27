# Integrations

Status: Draft · 2026-10-26

## API

- REST under `/api/v1`, described by an OpenAPI 3.1 document generated from the Go handlers (Huma) and served at `/api/v1/openapi.json` (and `.yaml`). The frontend client and the CLI are generated from it. `/healthz` answers liveness probes outside the API.
- Every response carries an `X-Request-ID`: the caller's when it is 1 to 64 letters, digits, `.`, `_` or `-`, a new one otherwise. It is in the request's log line and its trace.
- Cursor pagination, `Idempotency-Key` on every POST that creates something, ETags for optimistic concurrency on admin objects.
- Errors use RFC 9457 problem details, with field-level errors for validation located in the request body (`errors: [{location: "body.values.cpu", message: "…"}]`; blueprint inputs under `body.inputs`, action inputs under `body.actionInputs`, locator params under `body.params`). A request the definitions cannot plan (an expression that fails, a blueprint resource its type rejects) is a 422 whose `detail` says why.
- Deprecation: an endpoint stays at least two minor releases after it is marked deprecated, announced in the `Deprecation` and `Sunset` headers.

## Outgoing webhooks

Admins subscribe URLs to events (`request.submitted`, `request.approved`,
`request.applied`, `resource.drifted`…). Payloads are signed with HMAC-SHA256
(`Quai-Signature` header with timestamp), retried with backoff, and visible in a
delivery log.

## Notifications

| Channel | Notes |
| --- | --- |
| Email | SMTP; per-user preferences; daily digest option |
| Microsoft Teams | Incoming webhook (Workflows) for channel posts; a Teams app with Adaptive Cards for personal approval requests with Approve and Deny buttons |
| Slack | Slack app with interactive messages for approvals |

Approving from chat: the button opens a signed, short-lived link bound to the
approver and the change set hash. If the rule requires step-up authentication,
the link opens Quai and re-authenticates instead of approving directly.

## ITSM

- **ServiceNow:** change requests through the Table API or Change Management API. Standard change templates map to catalog items marked as pre-approved. Fields mapped: short description, justification (the reason), implementation plan (the diff summary), risk and impact (from rules), planned window (from change windows). See [delivery and approvals](05-delivery-and-approvals.md#itsm).
- **Jira Service Management:** same model through its REST API (later).
- Incident links: the break-glass flow requires an incident reference and can validate it against ServiceNow.

## Developer portals

- **Backstage:** a backend plugin that exposes Quai resources as catalog entities (kind `Resource`, with owner and system from Quai), and a scaffolder action `quai:request` so Backstage templates can request infrastructure through Quai's approvals instead of writing to Git directly.
- **Port:** a webhook-based self-service action and an exporter for resources.

## CLI

The same Go binary as the server (`quai`), distributed as a static binary and a
container image:

| Command | Purpose |
| --- | --- |
| `quai login` | OIDC device flow |
| `quai catalog list`, `quai request create -f request.yaml` | Request from the terminal or a script |
| `quai request approve <id>` | Approve with the same rules as the UI |
| `quai status <resource>` | Resource status and pending requests |
| `quai lint` | Validate definitions and resource files in a repository's CI (see [resource types](02-resource-types-and-generators.md#testing-a-type)) |
| `quai report` | Send a run report from a pipeline (see [runs](06-runs-status-and-drift.md)) |
| `quai schema from-terraform` | Draft a JSON Schema from module variables |
| `quai audit verify` | Verify the audit hash chain and checkpoints |

## AI agents (MCP)

An MCP server exposes Quai to coding and operations agents: list the catalog,
describe a type's form, plan a request, submit it, check its status. Agents
are principals with their own entitlements, and their requests go through the
same approvals as people's. This gives agents a governed path to
infrastructure instead of direct Git or cloud access.
