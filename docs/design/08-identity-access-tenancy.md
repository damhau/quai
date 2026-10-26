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

### Microsoft Entra ID (the pilot's IdP)

- Quai is registered as a confidential client (authorization code with PKCE). The secret or certificate for that registration is a Kubernetes Secret.
- **Prefer app roles over raw group claims.** Admins assign Entra groups (synced from on-premises AD in a hybrid setup) to Quai's app roles; the token carries a `roles` claim that Quai maps to teams and roles. This avoids Entra's group overage: a JWT lists at most 200 groups, beyond which Entra sends only a pointer and the groups must be fetched from Microsoft Graph.
- If raw group claims are used instead, configure the registration to emit only the groups assigned to the application. If an overage still occurs, Quai fetches memberships from Microsoft Graph when that egress is allowed, and otherwise refuses the login with an explicit message rather than silently dropping groups.
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

## Sessions and accounts

- Sessions expire after 8 hours of activity and 30 minutes idle (configurable), and are revoked on logout and on SCIM deactivation.
- Deactivated users' pending requests are cancelled; their pending approvals are dropped and approvers re-notified.
- Every login, token creation and role change is an audit event.
