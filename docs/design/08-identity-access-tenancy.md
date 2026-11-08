# Identity, access and tenancy

Status: Draft · 2026-10-26

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

- Users and their groups are read from ID token claims at each login. Group claims are mapped to Quai teams by a configurable mapping (`groups: [sg-quai-*]` → teams).
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
- **Teams:** own resources, instances and catalog items; map to IdP groups.
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

Catalog items declare who may request them:

```yaml
catalog:
  visibility: [group:developers, group:data-*]
  requesters: [group:developers]
  environments:                  # narrower entitlements per param value
    prod: [group:prod-requesters]
```

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
  of the owning team (the IdP group of the team's name), or a platform admin.
- Entitlements are group patterns (`group:data-*`) or names (`user:zoe`); an empty
  list does not restrict.

## Sessions and accounts

- Sessions expire after 8 hours of activity and 30 minutes idle (configurable), and are revoked on logout and on SCIM deactivation.
- Deactivated users' pending requests are cancelled; their pending approvals are dropped and approvers re-notified.
- Every login, token creation and role change is an audit event.
