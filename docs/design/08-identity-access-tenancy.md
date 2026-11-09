# Identity, access and tenancy

Status: Draft · 2026-11-09

## Authentication

| Client | Mechanism |
| --- | --- |
| Browser | OIDC authorization code flow with PKCE (`coreos/go-oidc` + `golang.org/x/oauth2`). The server keeps the tokens; the browser gets an `HttpOnly`, `Secure`, `SameSite=Lax` session cookie (`alexedwards/scs` with a PostgreSQL store). State-changing requests also need a CSRF token. |
| Behind an auth gateway | `proxy` mode, kept from the prototype: trust `X-Forwarded-User`, `-Email`, `-Groups` from oauth2-proxy or a similar gateway. Only valid when Quai is reachable exclusively through it; Quai refuses proxy mode if a shared secret header from the gateway is missing. |
| CLI | OIDC device authorization grant (`quai login`), then a short-lived session token. |
| Automation | API tokens: personal or service tokens, scoped (`requests:write`, `catalog:read`…), with expiry, hashed at rest, shown once. |
| Runners | Per-repository run tokens, scoped to `runs:write` for that repository only. |
| Git platform webhooks | Verified by the connector: HMAC signatures where the platform signs (GitHub), a secret header or basic auth where it does not (Azure DevOps service hooks, GitLab). Payloads are never trusted; Quai re-reads the object from the API before acting. |
| AI agents (MCP) | Service principals with their own entitlements; every request they make is attributed to the agent and to the human who configured it. |

The dev user switcher remains for local development only and is refused when
`QUAI_ENV=production`.

How it works in the server:

- `/auth/login` starts the OIDC login (state, nonce and a PKCE verifier kept in the
  session) and `/auth/callback` finishes it: state compared, code exchanged with the
  verifier, ID token verified (issuer, audience, expiry, signature, nonce), session
  token renewed against fixation. `next` only redirects to a path on Quai.
- The person's name comes from `nameClaim` (`preferred_username` by default), then
  `preferred_username`, then their email when the IdP vouches for it (below); without any of
  them the login is refused, naming the missing claims. Their groups come from `groupsClaim`
  (`groups`, or `roles` for Entra app roles); `auth.roles` maps groups to roles at a scope.
  Roles are recomputed from the groups on every request, so a mapping change applies at once.
- The session cookie is `quai_session`. `GET /api/v1/me` returns the session's CSRF token;
  every `POST`, `PUT`, `PATCH` or `DELETE` with a session must send it as `X-CSRF-Token`.
- API tokens look like `quai_…`, are sent as `Authorization: Bearer`, act with the groups
  their owner had when creating them, and are narrowed by their scopes (`catalog:read`,
  `inventory:read`, `requests:read`, `requests:write`). A token cannot create tokens.
- `POST /auth/logout` ends the session.

### Step-up

Some actions require a recent, and strong enough, authentication (P4-06): reviewing a request
whose approval asks for it ([design 05](05-delivery-and-approvals.md#step-up-authentication)),
breaking glass and signing off a break-glass review whenever `auth.stepUp` is configured
([break-glass](05-delivery-and-approvals.md#break-glass)), delegating one's approvals whenever
`auth.stepUp` is configured ([delegation](05-delivery-and-approvals.md#delegation)), and, with
`auth.stepUp.admin`, connecting, changing and disconnecting repositories, installing their webhooks
and generating their run tokens.

- **What counts** is what the identity provider said in the ID token of the person's last sign-in,
  kept in the session: its `auth_time`, no older than `auth.stepUp.maxAge` (five minutes by
  default) and no more than a minute in the future (the clocks' skew); its `acr`, exactly one of
  `auth.stepUp.acrValues` when they are set; its `acrs`, holding `auth.stepUp.authContext` (Entra
  ID) when it is set. Anything missing fails: OIDC Core requires `auth_time` when `max_age` is
  asked for, and a token without it, or without the `acr` or context asked for, satisfies no
  step-up though the person is signed in. The time Quai created the session never counts. An API
  token never steps up ([open question 49](../plan/open-questions.md#still-open)).
- **On the server**, when the action is made: `auth.StepUp.Require` checks the caller with the
  server's clock, records a refusal as `auth.step_up_refused` (what was attempted, on what, why)
  and returns a `403` problem of type `https://quai.dev/problems/step-up-required`, whose detail
  says why. Break-glass and its sign-offs call the same check (`request.break_glass`,
  `request.break_glass_sign_off` as what was attempted). `GET /api/v1/me` says whether the caller's
  authentication satisfies a step-up now, why not, until when, and whether repository changes
  need one, for the UI to ask before the action rather than after.
- **The round trip.** The UI's **Sign in again** goes to `/auth/login?stepUp=1&next=<the page>`:
  a login like any other (state, nonce and PKCE verifier bound to the session, ID token
  verified, session renewed) that also sends `prompt=login` and `max_age` (so that the provider
  authenticates the person again and says when), `acr_values` when configured, and, for Entra ID,
  the authentication context in the `claims` parameter
  (`{"id_token":{"acrs":{"essential":true,"value":"c1"}}}`). The browser comes back to `next`, a
  path on Quai validated when the login starts: it must start with a single `/`, and hold no
  backslash, no control character, and no `//` once percent-decoded, however many times; anything
  else is `/`. The login is audited with `stepUp` and the ID token's `acr` and `acrs`. With the
  development switcher, signing in again steps up; `acrValues` and `authContext` need `oidc`.
- **A review keeps** the `auth_time`, `acr` and `acrs` it was made with, and whether they
  satisfied the step-up then: that is what counts, not the session's later state.

## Directory

- Users and their groups are read from ID token claims at each login, and kept in the directory
  (the `users` table, P4-04): the name, the email when the IdP vouches for it (below), the groups,
  the ID token's `sub` and Entra ID's `oid`, the time of the login and of the authentication, and
  whether the person is deactivated. A person is their name, everywhere in Quai: a login whose name
  the directory holds for another `sub` is refused, since it would act as that person
  ([open question 45](../plan/open-questions.md#still-open)); a deactivated person cannot sign in,
  and their sessions and API tokens are refused from their next request (a session is checked
  against the directory at every request). Nothing erases a person yet
  ([open question 44](../plan/open-questions.md#still-open)).
- **Teams.** `auth.teams` maps groups to Quai teams
  ([open question 20](../plan/open-questions.md#still-open)): a list of `{group, team}`, where the
  group is a name or a pattern with one `*` (one or more characters, with their case) and a `*` in
  the team stands for what it matched (`{group: sg-quai-*, team: "*"}`: `sg-quai-shop` is the team
  `shop`). The first entry that matches a group decides its team. Without `auth.teams`, each group
  is the team of its own name, as before P4-04. With it, **only the groups an entry matches are
  teams**: a group no entry matches makes no team, so that a group that happens to share a team's
  name (a self-service group, another department's) does not make its members that team's owners
  and approvers; it still counts for `group:` entries and `auth.roles`. A last entry
  `{group: "*", team: "*"}` brings the fallback back. A person's teams are worked out from their groups on every request, as
  their roles are, and decide ownership by a team (`ownership: { team: shop }`), `team:` owner
  entries (for `owners` approvers and operating) and the inventory's "owned by me". `group:`
  entries match groups, never teams. A review keeps the reviewer's teams, as it keeps their groups;
  a review from before P4-04, which kept only groups, has the teams `auth.teams` makes of them now.
- **Access report.** Auditors and platform admins read every person in the directory, and every
  owner of an API token that still works, with their groups, teams, roles by scope and the groups
  that give each, their tokens (a token acts with the groups its owner had when creating it, so it
  can hold a role its owner lost: [open question 46](../plan/open-questions.md#still-open)), their
  last login and their state: a page under Admin and a CSV download
  (`GET /api/v1/access-report`, `/export`) for periodic access reviews. Teams and roles are those
  the current configuration gives the groups of the person's last login. CSV cells starting with
  `=`, `+`, `-`, `@`, a tab or a carriage return get a leading apostrophe, so that a spreadsheet does
  not run a name or a group from the IdP as a formula, and every cell is quoted.
- **SCIM 2.0 provisioning** (optional, P4-15) keeps people and groups current between logins and
  deactivates leavers at once, with their sessions, tokens, requests and approvals
  ([below](#scim-provisioning)).
- **Manager** (for `approvers: [manager]`, P4-11). Each sign-in keeps the person's manager in the
  directory as an immutable identifier of the identity provider, never a name or an email: the ID
  token's claim `auth.oidc.managerClaim` (the manager's `sub`, or their Entra ID `oid`), or Microsoft
  Graph's `/me/manager` with `auth.graph.manager` (the manager's `oid`); the two are exclusive. For a
  person SCIM provisions, SCIM's `manager` alone counts, and sign-ins leave it
  ([below](#scim-provisioning)). Where it came from is kept with it, and when there is none,
  why: no claim, a value that is not one identifier, no manager in Graph, a manager who is a contact
  rather than a user, a Graph lookup that failed, a sign-in from another tenant. A sign-in always
  replaces them: a failed lookup leaves no manager, never the one found before, since whoever
  Graph names now may differ. A change is audited (`auth.manager_changed`). When a request is
  planned, the identifier is resolved to the one person of the directory whose `oid` (or, from a
  claim, `sub` or `oid`) it is; none, several, a deactivated person or the requester themselves is
  no manager, with the reason ([open question 69](../plan/open-questions.md#still-open)). The
  manager is therefore as of the requester's last sign-in, and must have signed in once. Development
  users have none.
- Owners named by a Git platform (a `CODEOWNERS` entry such as `@alice` or `@org/team`; an Azure
  DevOps reviewer such as `alice@acme.ch` or `[Project]\Platform`), for the `codeowners` approver
  kind and `ownership.codeowners`, match Quai in this order
  ([open question 9](../plan/open-questions.md#decided), [29](../plan/open-questions.md#still-open)):
  1. the repository document's mapping, `spec.codeowners.mapping` (platform owner →
     `group:<name>` or `user:<name>`, keys without regard to case or a leading `@`): the only way
     a platform group (`org/team`, `[Project]\Name`) matches;
  2. an owner written as an email address (Azure DevOps account names on Services, `CODEOWNERS`
     email entries): the Quai user with that email;
  3. a user's email from the platform: on Gitea, the account's email unless it is a `noreply`
     placeholder; on GitHub, the user's emails in the organization's verified domains
     (`organizationVerifiedDomainEmails`, none when the organization has no verified domain,
     [open question 17](../plan/open-questions.md#still-open)).

  Entries that match no one are shown, as the platform writes them, on the plan, the request and
  the resource. A Quai user's email is the IdP's `email` claim **only when the IdP vouches for
  it**: `email_verified` or Entra ID's optional claim `xms_edov` is the boolean `true`. Otherwise
  (false, absent, any other value) the user has no email: no owner or `email:` entry matches them
  by email, and their commits are authored as `<name>@users.quai`
  ([open question 30](../plan/open-questions.md#still-open)). Development users' emails, set by
  the administrator in `auth.dev.users`, are trusted. An approver's email is kept with their
  review.

### Microsoft Entra ID (the pilot's IdP)

- Quai is registered as a confidential client (authorization code with PKCE). The secret or certificate for that registration is a Kubernetes Secret.
- **Prefer app roles over raw group claims.** Admins assign Entra groups (synced from on-premises AD in a hybrid setup) to Quai's app roles; the token carries a `roles` claim that Quai maps to teams and roles. This avoids Entra's group overage: a JWT lists at most 200 groups, beyond which Entra sends only a pointer and the groups must be fetched from Microsoft Graph.
- If raw group claims are used instead, configure the registration to emit only the groups assigned to the application. If an overage still occurs (the ID token names a source for its groups in `_claim_names`, or says `hasgroups`), Quai reads the groups from Microsoft Graph with `auth.graph.groups`, and otherwise refuses the login with an explicit message rather than silently dropping groups. As built (P4-11, [open question 68](../plan/open-questions.md#still-open)):
  - **Delegated, from the sign-in itself.** The login asks for Graph's delegated permissions as scopes (`https://graph.microsoft.com/User.Read` for the groups, `.../User.Read.All` for the manager), and Quai uses the access token the code exchange returns, in memory, during the callback, for that person only (`/me`); it is never stored, logged or put in an error. No client secret with tenant-wide application permissions: Graph's `/manager` does not support them, and the sign-in's token reads only what the person may read.
  - **Only the configured Graph.** Quai calls `auth.graph.baseURL` (https, `https://graph.microsoft.com` by default, a national cloud's root otherwise) through `network`, with TLS verified, and nothing else: it ignores the endpoint `_claim_sources` names, follows no redirect, and follows no `@odata.nextLink`. The ID token's `tid` must equal `auth.graph.tenantId` before Graph is called.
  - **Groups:** `POST /v1.0/me/getMemberObjects` with `securityEnabledOnly: true`, which Microsoft names for an overage and which `User.Read` allows: the object IDs of the person's security groups, transitive memberships included (so `auth.oidc.groupsClaim` must be `groups`, and `auth.roles` and `auth.teams` name groups by object ID). Its documentation describes no paging; an answer with `@odata.nextLink` is partial, and refused. At most 10,000 groups, an answer of at most 1 MiB, two retries of a 429, 503 or 504 waiting as `Retry-After` says (at most five seconds), twenty seconds in all.
  - **Fail closed.** An overage Graph cannot resolve (an error, throttling beyond the retries, a token or permission Graph refuses, a partial answer, too many groups, another tenant) refuses the login, logged and counted (`quai.graph.lookups`): letting the person in without their groups would hide what they may and may not do.
- **Emails:** Entra ID sends no `email_verified`, and a guest's `email` can come from their home
  tenant. For owners to match by email, add the optional claim `xms_edov` (email domain owner
  verified) to the ID token in the registration's token configuration; without it, Quai users have
  no email, and a person named as a code owner matches only through a `user:` entry of the
  repository's mapping (groups match through `group:` entries either way).
- The `manager` attribute is not in tokens; with `auth.graph.manager` it is read at each login from Microsoft Graph (`GET /v1.0/me/manager`, delegated `User.Read.All`, which needs an administrator's consent), with the same token, limits and tenant check as the groups. A manager lookup that fails does not refuse the login: the person has no manager until a login finds one, logged and counted, and their requests' manager rules say why. Without Graph or a claim, `manager` approvers are unavailable and requests say so.
- **Step-up:** Entra Conditional Access *authentication contexts* express "MFA required for this action". Quai requests the context (`auth.stepUp.authContext`) for step-up approvals, break-glass and, with `auth.stepUp.admin`, repository changes, checks the `acrs` claim in the returned ID token, and combines it with `max_age` and `prompt=login` for a recent sign-in. The registration must add the optional claims `auth_time` and `acrs` to the ID token, and the Conditional Access policy on the context should set the sign-in frequency to every time ([operations guide](../operations/deployment-and-operations.md#step-up-authentication)).

### SCIM provisioning

As built (P4-15, [open questions 83 to 88](../plan/open-questions.md#still-open)). The tests send
the requests Microsoft Entra ID's provisioning service sends, as Microsoft documents them; it is
**not verified against a real Entra ID tenant**. Setting it up: the
[operations guide](../operations/deployment-and-operations.md#provisioning-with-scim).

- **Endpoint and token** ([open question 83](../plan/open-questions.md#still-open)). With
  `auth.scim` (which needs `auth.mode: oidc`), Quai serves `/scim/v2`: `Users`, `Groups`,
  `ServiceProviderConfig`, `ResourceTypes` and `Schemas`. The provisioning service authenticates
  with a bearer token of its own, `quai_scim_` and 64 hex characters, made by `quai scim-token`
  outside Quai. Quai keeps only its SHA-256, in the secret each `auth.scim.tokens` entry names (one,
  and the next while rotating; at most four), and compares it in constant time with each. Nothing
  else is taken there, no session and no API token, and the rest of the API does not take a SCIM
  token. A token is revoked by removing it from the configuration or changing its secret (read
  again within a minute). The access report shows each token's last use, to the minute. Everything
  SCIM changes is in the audit log as the provisioning's (actor type `scim`, the token's name),
  never a person's.
- **Bounds.** 50 requests a second (bursts of 100), then 429 with `Retry-After`. A body of at most
  1 MiB, `application/scim+json` or `application/json`: one JSON object, attribute names in any case
  and none twice, at most 100 in an object. Strings of at most 256 characters, without control
  characters. A PATCH has 1 to 100 operations; a request adds or removes at most 1,000 members.
  Pages are `startIndex` and `count`, at most 200 (100 by default), in name order. **Filters** are
  what Entra ID sends, nothing more: `attribute eq "value"` comparisons joined by `and`, each
  attribute once, at most four, 1,024 bytes, of `userName`, `externalId`, `id` and `manager` for
  users and `displayName`, `externalId`, `id` and `members` for groups; anything else is 400
  `invalidFilter`. Their values are only ever query parameters. Errors are SCIM's (RFC 7644 §3.12)
  and never carry an internal error. No PUT, no Bulk (501), no sorting, no ETags.
- **What Quai keeps** ([open question 88](../plan/open-questions.md#still-open)). Of a user:
  `userName`, `externalId`, `active` and the enterprise extension's `manager`. Other attributes
  (emails, names, roles, phone numbers) are accepted in the forms Entra ID sends them (paths with a
  filter such as `emails[type eq "work"].value`, a PATCH without a path, booleans as `"True"` and
  `"False"`) and not kept: the ID token gives the email at sign-in. Of a group: `displayName`
  (unique, in any case), `externalId` (set once) and its members.
- **Binding** ([open question 84](../plan/open-questions.md#still-open)). `externalId` is required
  and must be the account's immutable identifier: Entra ID's object ID (the default mapping uses
  `mailNickname`, which must be changed to `objectId`), or the ID token's `sub` with another
  identity provider. A provisioned user's `userName` becomes their Quai name, for good: a rename
  changes their `userName`, not the person. Someone who signed in before is adopted only when their
  identifiers in the directory match the `externalId`. A name the directory holds for another
  account, a name equal to another but for case, another user's `externalId`, a reserved name: 409
  `uniqueness`, at creation and at a rename alike. `externalId` cannot change (400 `mutability`). At
  sign-in, a provisioned person is found by their `oid` (else `sub`) and acts under the name SCIM
  bound, whatever name the ID token gives; before their first sign-in, that must be their Quai name
  or their current `userName`. A sign-in with a provisioned name and another identifier, or a
  provisioned identifier and another name, is refused (403, saying why). A DELETE deactivates the
  person, takes them out of the groups SCIM provisions and unbinds them (a GET then answers 404):
  their row and name stay, so a new person given the old `userName` is refused (409), never
  inheriting the old one's requests, reviews, delegations or groups, and the same `externalId`
  provisioned again gets the account back.
- **Groups** ([open question 85](../plan/open-questions.md#still-open)). A provisioned group's name
  in Quai is its `externalId` (`auth.scim.groupName: externalId`, the default: Entra ID maps the
  group's object ID to it, which is how the ID token's `groups` claim names groups) or its
  `displayName`. A provisioned person's groups are those of their last sign-in, less every group
  SCIM has provisioned (its name, even after the group is deleted) that does not list them: SCIM
  never gives a group the ID token does not, and takes one away at once. A session or an API token
  acts with the groups it was issued with that the person still has, checked at every request: out
  of the admin group, a person is no admin at their next request, by session or token; back in, a
  session or token issued with the group has it again, others from the next sign-in. A change of a
  person's groups is audited with the groups and grants before and after (`auth.groups_changed`),
  as are the memberships (`scim.members_changed`). People SCIM does not provision keep their ID
  token's groups.
- **Deactivation** ([open question 86](../plan/open-questions.md#still-open)). `active: false`
  (`"False"` from Entra ID) or a DELETE, at once and completely: the person's sessions end at their
  next request (the session's epoch no longer matches), their API tokens are revoked, the
  delegations they gave or received are revoked, their requests not yet delivered are cancelled
  ("its requester was deactivated"), and their reviews of requests still waiting for approval, and
  reviews made on their behalf, are voided: shown as void, never counted again, the request's
  approvers notified again (`reviews_voided`). Approvals of requests already approved or delivered
  stand. A request waiting for them as its approver (its requester's manager) waits until it
  expires or is cancelled. Chat links need the person's session, so they stop working, and no
  notification is sent to them. They cannot sign in. Each effect is an audit event, the
  provisioning's. `active: true` reactivates the account: the person signs in again, and nothing
  revoked comes back. SCIM decides: it reactivates someone an administrator deactivated by hand.
- **Manager** ([open question 87](../plan/open-questions.md#still-open)). For a provisioned person,
  the enterprise extension's `manager` (the manager's SCIM `id`) is the manager, whatever
  `auth.oidc.managerClaim` or `auth.graph.manager` say for others; sign-ins do not change it. It
  may name someone provisioned later; until then, and when the manager is deactivated or is the
  requester, there is no manager, with the reason, as for the other sources. A change is audited
  (`scim.user_changed`).
- **Out of reach** ([open question 88](../plan/open-questions.md#still-open)). The names in
  `auth.scim.reserved` (the identity provider's emergency, break-glass accounts) and the development
  users' names: SCIM never creates or renames to them (409), nor changes, deactivates or deletes
  them (403), whatever the case. The `break-glass` role is a group's, like any other: a provisioned
  person who leaves loses it.

## Tenancy

- **Organization:** one per installation in v1. The data model carries an `org_id` everywhere so multi-organization installs (managed service providers) need no migration later.
- **Teams:** own resources, instances and catalog items; map to IdP groups through `auth.teams`
  ([directory](#directory)).
- **Ownership:** a resource's owner is a user or a team (see [resource types](02-resource-types-and-generators.md#ownership)). Team ownership is preferred: it survives people leaving.

## Authorization

### Roles

| Role | Can |
| --- | --- |
| **Viewer** | Browse catalog, inventory, requests within their visibility |
| **Requester** | Request catalog items they are entitled to |
| **Operator** | Run day-2 actions on resources owned by them or their teams |
| **Approver** | Not a role: eligibility comes from approval rules |
| **Catalog author** | Preview definitions from branches; manage catalog presentation; cannot bypass Git review of definitions |
| **Repository admin** | Manage a repository connection, its delivery and runner settings |
| **Platform admin** | Everything in Quai's configuration; operate any resource; still cannot approve their own requests |
| **Auditor** | Read-only access to every request, the audit log and evidence bundles |
| **Break-glass** | Emergency merge without approvals, under the controls in [delivery and approvals](05-delivery-and-approvals.md#break-glass) |

Roles are granted to IdP groups at a scope: global, team, repository or catalog
item.

**Break-glass** (P4-17) is its own role: being a platform admin does not give it, and it gives
nothing else (a member reads requests as their other roles allow). It applies at its scope (a
member with it for one repository breaks glass there only), in a browser session only: API tokens
carry no scope for it. It never lets a person break glass for their own request, one whose
requester delegated their approvals to them, or one whose definitions they authored within the
separation of duties' period ([open question 94](../plan/open-questions.md#still-open)). Its
post-incident review is signed off by platform admins, auditors and the request's owners (as an
`owners` rule matches them), and listed for global platform admins and auditors
([open question 27](../plan/open-questions.md#still-open)).

### Entitlements

Catalog items, types and blueprints alike, declare who sees them and who may request them, in
`spec.entitlements`:

```yaml
spec:
  entitlements:
    visibility: [group:developers, group:data-*]   # who sees the item in the catalog
    requesters: [group:developers]                 # who may request it, among those
    environments:                                  # narrower requesters per environment
      prod: [group:prod-requesters]
```

- Entries are group patterns (`group:developers`, `group:data-*`, matched with Go's `path.Match`
  against the person's IdP groups, never their teams) or people (`user:zoe`). An empty or absent
  list does not restrict. `quai lint` refuses any other entry (`team:shop`, a bare name, a
  malformed pattern, a pattern in a `user:` entry).
- Requesting needs to be in `visibility` and in `requesters`. `environments` is keyed by the value
  of the type's `env` param, or the blueprint's `env` input, and narrows the requesters for that
  value: a request for prod needs to be in `requesters` and in the prod list, which never widens
  `requesters`. The value matches without regard to case or surrounding spaces, as the param is
  written. `quai lint` refuses `environments` on an item with no `env` param or input, an empty
  list, two keys naming the same environment, and a key that is not among the param's options or
  the input's `enum` when the definition lists them.
- A blueprint's request needs the blueprint's entitlements, for its `env` input, and, for each
  resource it creates, that resource's type's, for the resource's `env` param: a blueprint does not
  open a type to more people ([open question 47](../plan/open-questions.md)).
- The catalog leaves out the items hidden from the caller. Their form (`get-type`,
  `get-blueprint`), plans and requests are refused with 403 and the reason ("this catalog item is
  not visible to your groups", "… is not open to your groups", "… in prod", or "type database
  is …" for a type a blueprint creates), as is a request for an environment whose list does not
  name the caller. The API decides; the catalog and forms only show the lists, and what the
  caller may request in each environment.
- Entitlements are about requesting new things. Owners and those who may read a resource or an
  instance keep its page, its day-2 actions, updates and deletion whatever the lists say
  (`get-type?resource=<key>`, `get-blueprint?instance=<name>` describe the item for them;
  [open question 48](../plan/open-questions.md)).
- Platform admins see and request everything; auditors see every item and request none; catalog
  authors describe every item of their repositories (previews and the designer show the lists).
- Changing an item's entitlements changes its definition: open requests made under the old lists
  are refused at delivery as stale, like any change to the definitions they were planned from.

### Evaluation

One function decides every access question:
`Authorize(ctx, principal, action, object) (Decision, error)`, with a reason
on denial. It lives in one Go package with table-driven tests per role and
scope. Every API handler and worker job goes through it; a lint check fails the
build if a handler under `internal/api` does not call it. Externalizing the
policy to OPA or Cedar later is possible behind the same function.

The check is a test in `internal/api`: every operation registered with Huma must
call `authz.Authorize`, directly or through a function of the package, unless the
comment just above its registration says `authz: public` (only `/api/v1/meta` so
far). Decisions the roles do not spell out:

- A platform admin can do everything except use break-glass, which needs its own
  role and never applies to one's own request.
- Anyone can read and cancel their own requests; reviewing needs access to the
  request, and the request's approval rules decide whose review counts.
- Operating a resource needs the operator role and ownership: the owner, a member
  of the owning team (in a group `auth.teams` maps to it; without `auth.teams`, in the group of its
  name), or a platform admin.
- The access report needs the auditor role (or platform admin), at the global scope; an API token
  cannot read it.
- Anyone signed in delegates their own approvals and revokes their own delegations, in a browser
  session: no scope allows it to an API token. Revoking someone else's needs the platform admin
  role; no one makes a delegation for someone else
  ([delegation](05-delivery-and-approvals.md#delegation)).
- Entitlements are group patterns (`group:data-*`) or names (`user:zoe`); an empty
  list does not restrict ([entitlements](#entitlements)).

## Sessions and accounts

- Sessions expire after 8 hours of activity and 30 minutes idle (configurable), and are revoked on
  logout and on deactivation: every request checks the person in the directory, so a deactivated
  person's session ends at their next request, and a deactivation by SCIM ends it for good.
- A deactivation by SCIM cancels the person's requests not yet delivered, voids their approvals of
  requests still waiting (approvers notified again), and revokes their API tokens and delegations
  ([SCIM](#scim-provisioning)).
- Every login, token creation and role change is an audit event: `auth.login`, `token.created`,
  and `auth.grants_changed` when a login's grants differ from the person's previous login.
