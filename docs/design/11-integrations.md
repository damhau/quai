# Integrations

Status: Draft · 2026-11-09

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
| Email | SMTP over TLS only: implicit TLS (port 465) or STARTTLS (587), which the server must offer (no fall back to plaintext); the certificate verified against the system's CAs and `network.caFile`; PLAIN authentication, or LOGIN when the server offers only that, with the password a secret reference. Text and HTML parts (the HTML from `html/template`), with the headline, the request's title, reason, diff summary (files and lines added and removed), the plan when there is one (summary, cost, policies, resource addresses and actions) and a link from `server.publicURL`. Headers made from people's names, titles and reasons are folded to one line and encoded (RFC 2047). Sent to a person's verified email from the directory (P4-07, [what is sent to whom](05-delivery-and-approvals.md#notifications)). Each person chooses on their profile page, per kind, at once, in a daily digest (one message a day at their hour, in their time zone) or not at all; alerts always go at once (P4-08) |
| Microsoft Teams | **Channel posts** through a Workflows webhook ("When a Teams webhook request is received"), one per channel in `notifications.teams.channels`, the URL a secret reference since its signature is the only credential: the events a channel chooses (kinds of notification and `alert`), for the repositories and owning teams it names, as an Adaptive Card with identifiers, the state and a link only, never a reason, diff or plan ([question 64](../plan/open-questions.md#still-open)). **Personal cards** from a bot (`notifications.teams.bot`: a single-tenant Entra ID application with a client secret, its Teams app installed for the people): a proactive message to the person's Entra ID object ID (`oid`, from their sign-in; without one they are skipped and the request says so). An approval needed is a card with the request, its diff summary and plan (as email, [question 54](../plan/open-questions.md#still-open)) and **Approve** and **Deny**, which open [a signed link](#approving-from-chat); the card is updated, without its buttons, once the request no longer waits for that person. Other kinds and digests are cards without buttons. The bot's tokens come from Entra ID's token endpoint (client credentials, scope `https://api.botframework.com/.default`) and are kept in memory until five minutes before they expire. Cards are Adaptive Cards 1.2 built from Go structs, people's text in `TextRun`s (never Markdown), with `Action.OpenUrl` only, under 24 KB; nothing comes in from Teams: Quai has no messaging endpoint. Every endpoint is https, from the configuration only, called through `network` with TLS verified; a webhook's URL never appears in an error or a log (P4-09, [question 63](../plan/open-questions.md#still-open)) |
| Slack | One Slack app, installed in one workspace (`notifications.slack`: its bot token `xoxb-…` and signing secret as secret references, the Web API at `apiURL`, https, by default `https://slack.com/api`). **Direct messages** to people: the account found with `users.lookupByEmail` from the person's verified email in the directory ([question 30](../plan/open-questions.md#still-open)), and written to only when it is an active full member of the app's workspace (or of its Enterprise organization): a guest, a bot, an account of another workspace, one whose email is not confirmed, or none, and the notification is skipped with the reason ([question 66](../plan/open-questions.md#still-open)); then `conversations.open` and `chat.postMessage`. An approval needed says what email says (questions [54](../plan/open-questions.md#still-open) and [55](../plan/open-questions.md#still-open)) with **Approve** and **Deny** buttons that open [a signed link](#approving-from-chat) (channel `slack`); the message is updated with `chat.update`, without those buttons, once the request no longer waits for that person. **Channel posts** with `chat.postMessage` to the channel IDs in `notifications.slack.channels`, routed as Teams' (events, repositories, owning teams), with identifiers, the state and a link only ([question 64](../plan/open-questions.md#still-open)); the app must be in the channel. Messages are Block Kit built from Go structs with `encoding/json`: every text in a block is a `plain_text` object, which Slack does not format, so `<!channel>`, `<@U…>` or `<https://…\|…>` in a title or reason show as written; the fallback `text`, which Slack formats, has `&`, `<` and `>` escaped; links do not unfurl; a message stays under 50 blocks and 24 KiB, leaving files and resources out when it must. The token goes in the `Authorization` header of every call, never in a URL, a body, an error or a log; a 429 or `ratelimited` is tried again after `Retry-After`, within limits. **Interactions:** Slack sends a request to `POST /slack/interactions` for every click, even on a button that opens a link; Quai verifies it with the signing secret and acknowledges it, and does nothing else ([question 67](../plan/open-questions.md#still-open), [the threat model](09-security-and-compliance.md#threats-and-mitigations)); without a signing secret the route is absent (404), and the app's interactivity should point nowhere (P4-10) |

Each channel implements `notify.Channel`: its name, a person's address on it
(none: the notification is recorded as skipped, and the request says whom it
does not reach), and sending a message, which every channel renders from the
same content, a digest's included. A channel that updates what it sent
(`notify.Updater`, Teams and Slack) gets an approval card's buttons and is told
when the card no longer asks for a decision; a channel that posts to a chat
channel (`notify.Poster`) adds rows per channel (`teams-channel:<name>`,
`slack-channel:<name>`) to the fan-out. A channel that finds, when sending,
that a person cannot be reached there (`notify.Unreachable`: Slack has no
member with their email) has the notification skipped with the reason, not
tried again. A notification is one row per event, recipient and channel, and
preferences are per channel: a new channel joins `config.NotificationChannels`,
the API's `channel` enum, and `notifications.Channels()` when configured, and
people choose for it as for email.

### Approving from chat

A link in a chat message is not a credential ([question 22](../plan/open-questions.md#still-open)).
Approve and Deny open `/requests/<id>?link=<token>`, where the token, made by
`internal/chatlink`, binds the request, the approver, the
decision, the change set hash the card showed and the channel, until it
expires: a day, and never past the decision it was made for, since it is
refused once the request is decided or its content changes
([question 61](../plan/open-questions.md#still-open)). Its MAC is HMAC-SHA256,
over a canonical encoding of those fields and the expiry, with a key used for
nothing else (`notifications.links.key`), compared in constant time.

- **Opening it changes nothing.** The page asks `POST
  /api/v1/requests/{id}/chat-link` which decision the link opens for the person
  signed in, and shows the request with that decision ready: **Confirm
  approval**, or **Confirm denial** with the comment a denial needs.
- **Confirming is the review**, `POST /api/v1/requests/{id}/review` with the
  link, in a browser session with its CSRF token and every check of a review:
  that the person may review, a step-up when the request needs one (the page
  asks them to sign in again and comes back to the link), the change set hash.
  The link's checks come on top, first: its MAC, its expiry, the request, the
  person signed in being the one it was sent to, the decision, the request
  still waiting for approval, and its change set the current one.
- **A refusal says why and is audited** (`request.link_refused`, with its code,
  and what the MAC vouches for of the link); the page then shows the request
  as without the link. An API token never opens a link
  ([question 62](../plan/open-questions.md#still-open)).

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
| `quai audit export` | Write the audit log in JSON lines, its checkpoints then its events, to verify it offline ([audit log](09-security-and-compliance.md#audit-log)) |
| `quai audit verify` | Verify an export of the audit log offline: the hash chain, the checkpoints' signatures with the public keys (`--key`), and the audit repository's checkpoints (`--anchors`); names the first `seq` that fails |
| `quai audit verify --bundle` | Verify a request's evidence bundle offline (its JSON download, a file or `-`): its events, the segments of the log to checkpoints signed with a `--key`, the clear personal values against their pseudonyms, the change sets, and the summary against the events; exits with status 1 on a failure, and on an unproven event unless `--allow-unproven` ([evidence bundle](09-security-and-compliance.md#evidence-bundle)) |

## AI agents (MCP)

An MCP server exposes Quai to coding and operations agents: list the catalog,
describe a type's form, plan a request, submit it, check its status. Agents
are principals with their own entitlements, and their requests go through the
same approvals as people's. This gives agents a governed path to
infrastructure instead of direct Git or cloud access.
