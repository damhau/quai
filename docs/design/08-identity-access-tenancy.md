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

Some actions (approving prod changes, break-glass, admin changes) can require a
recent authentication. Quai checks the ID token's `auth_time` against a
configured maximum age and, if the IdP supports it, `acr` for MFA. If either is
too old or too weak, the UI redirects through the IdP with `max_age` (and
`acr_values`) and resumes the action afterwards.

## Directory

- Users and their groups are read from ID token claims at each login, and kept in the directory
  (the `users` table, P4-04): the name, the email when the IdP vouches for it (below), the groups,
  the ID token's `sub` and Entra ID's `oid`, the time of the login and of the authentication, and
  whether the person is deactivated. A person is their name, everywhere in Quai: a login whose name
  the directory holds for another `sub` is refused, since it would act as that person
  ([open question 45](../plan/open-questions.md#still-open)); a deactivated person cannot sign in.
  Nothing erases a person yet ([open question 44](../plan/open-questions.md#still-open)).
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
- SCIM 2.0 provisioning (optional) keeps users and groups current between logins and deactivates leavers immediately, which matters for pending approvals.
- The `manager` attribute (for `approvers: [manager]`) comes from a claim or from SCIM.
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
- If raw group claims are used instead, configure the registration to emit only the groups assigned to the application. If an overage still occurs, Quai fetches memberships from Microsoft Graph when that egress is allowed, and otherwise refuses the login with an explicit message rather than silently dropping groups.
- **Emails:** Entra ID sends no `email_verified`, and a guest's `email` can come from their home
  tenant. For owners to match by email, add the optional claim `xms_edov` (email domain owner
  verified) to the ID token in the registration's token configuration; without it, Quai users have
  no email, and a person named as a code owner matches only through a `user:` entry of the
  repository's mapping (groups match through `group:` entries either way).
- The `manager` attribute is not in tokens; it is read from Microsoft Graph (`/users/{id}/manager`) at login, or left out if Graph is not allowed (then `manager` approvers are unavailable and Quai says so).
- **Step-up:** Entra Conditional Access *authentication contexts* express "MFA required for this action". Quai requests the context for prod approvals and break-glass, checks the `acrs` claim in the returned token, and combines it with `max_age` for recent sign-in.

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
- Entitlements are group patterns (`group:data-*`) or names (`user:zoe`); an empty
  list does not restrict ([entitlements](#entitlements)).

## Sessions and accounts

- Sessions expire after 8 hours of activity and 30 minutes idle (configurable), and are revoked on logout and on SCIM deactivation.
- Deactivated users' pending requests are cancelled; their pending approvals are dropped and approvers re-notified.
- Every login, token creation and role change is an audit event: `auth.login`, `token.created`,
  and `auth.grants_changed` when a login's grants differ from the person's previous login.
