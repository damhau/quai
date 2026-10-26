# Delivery and approvals

Status: Draft · 2026-10-26

How a planned change reaches a repository, and how Quai's approval is made
binding.

## Delivery modes

| Mode | What happens | Use for |
| --- | --- | --- |
| `pull-request` (default) | Branch + PR at submit; CI plans while approvals are pending; Quai merges when approved and green. | Every production repository. |
| `direct` | Quai commits to the branch after approval, as the prototype does. No CI plan before approval. | Sandboxes, demos, repositories without a supported provider API. |

See [ADR 0008](../adr/0008-pull-requests-by-default.md).

## Pull request protocol

```mermaid
sequenceDiagram
  actor U as Requester
  participant Q as Quai
  participant G as Git provider
  participant C as CI / runner
  actor A as Approver
  U->>Q: submit request
  Q->>G: push branch quai/r-1042-web01-resize
  Q->>G: open PR, labels quai, env:prod
  Q->>G: check quai/approval = pending
  G-->>C: PR event
  C->>C: terraform plan, policy, cost
  C-->>Q: run report (plan summary)
  C-->>G: checks plan, policy = success
  Q-->>A: notify (diff + plan)
  A->>Q: approve
  Q->>Q: all rules satisfied, window open
  Q->>G: check quai/approval = success
  Q->>G: merge (squash)
  G-->>C: push to main
  C->>C: terraform apply
  C-->>Q: run report (apply succeeded)
  Q-->>U: notify applied
```

### Branch, commit, pull request

- **Branch:** `quai/r-<request number>-<slug>`, created from the current head of the target branch.
- **Commit:** one per PR. Author: the requester (name and email from the IdP). Committer: Quai. Signed (GitHub App API commits are verified; elsewhere SSH or GPG signing with a key from the secret store). Message:

  ```
  quai: resize infra-live/vm/prod/web01

  Black Friday traffic; see INC-5512.

  Quai-Request: #1042 3b6f2a8c-…
  Quai-Change-Set: 9f2c41…
  Quai-Stack: envs/prod
  Requested-by: Bob Builder <bob@acme.ch>
  ```

  `Approved-by:` trailers cannot be in this commit (approval happens after it). They are added to the squash-merge commit message, or recorded on the PR as a final comment when the merge method keeps the original commit.
- **Pull request:** title as the commit subject; body with the reason, a link to the request, the resources touched, and a machine-readable marker (`<!-- quai:request=3b6f2a8c… -->`) so webhook events map back to the request. Labels: `quai`, plus labels from the type (`env:prod`, `stack:envs/prod`).

### The `quai/approval` check

| State | When |
| --- | --- |
| `pending` ("Waiting for 1 of 2 approvals") | PR opened, rules not yet satisfied |
| `success` ("Approved by alice, fiona") | All rules satisfied and change window open |
| `failure` ("Rejected by nora: CIDR overlaps") | A reviewer denied, or the request expired or was cancelled |
| `neutral` ("Stale: re-plan required") | The owned region changed on the base branch |

Branch protection on the target branch must require `quai/approval`. Quai
verifies this at registration and hourly; see
[repository connections](01-repository-connections.md#health). On GitHub the
required check is pinned to the Quai app so no other actor can satisfy it.

### Merge

- `merge: auto`: when `quai/approval` is success and every check in `waitForChecks` is green, Quai merges with the configured method. If the provider has a merge queue or merge train (GitHub merge queue, GitLab merge trains; availability depends on the provider's edition), Quai enqueues instead of merging directly.
- `merge: manual`: Quai sets the check and notifies the requester; a human merges. Useful where the customer insists on a human pressing the button.
- After merge, Quai deletes the branch and records the merge commit on the request.

## Approvals bound to content

An approval is valid only for one **change set hash**: a SHA-256 over the
rendered file edits, the definitions commit and the region fingerprints. It
changes if anything that was reviewed changes.

| Event | Effect |
| --- | --- |
| The base branch moves, owned regions unchanged | Quai rebuilds the branch on the new base by re-applying the edits. Same hash: approvals kept; CI re-plans. |
| The base branch moves and an owned region changed | Request becomes **stale**. Approvals void, check `neutral`, requester notified to re-plan. |
| Someone pushes to the `quai/` branch | Approvals void; the PR shows "modified outside Quai". Quai re-plans from the branch content only if the new diff still stays inside the owned regions; otherwise the request is closed. |
| The PR is closed outside Quai | Request cancelled. |
| The PR is merged without `quai/approval` success | Should be impossible with enforcement. Recorded as a **bypass** audit event and alerted. |
| The type or blueprint definition changes before merge | Request becomes stale. |

## Approval policy

Rules come from three places, evaluated together:

1. **Repository floor rules** (admin-owned, cannot be removed);
2. **Type rules** (in the IaC repository);
3. **Blueprint rules** (in the catalog), for blueprint requests.

```yaml
approval:
  rules:
    - name: production
      when: "params.env == 'prod'"
      approvers: [group:platform-admins]
      min: 1
    - name: owners
      approvers: [owners]                      # the resource's owning team
    - name: cost
      when: "plan.cost.monthlyDelta > 500"     # available once the plan run has reported
      approvers: [group:finops]
    - name: change-ticket
      when: "params.env == 'prod'"
      external: servicenow                     # satisfied by an approved change request
  expiry: 7d
  stepUp: "params.env == 'prod'"               # approvers must have re-authenticated recently
```

### Approver kinds

| Approver | Resolved from |
| --- | --- |
| `group:<name>` | IdP group |
| `user:<name>` | A named user |
| `owners` | The resource's owner (user or team) |
| `codeowners` | `CODEOWNERS` entries for the paths in the change set |
| `manager` | The requester's manager, from the IdP attribute |
| `external: <integration>` | An external system (ServiceNow change request, generic webhook) that returns approved or rejected |

### Separation of duties

- A requester never approves their own request, even as an admin.
- One person counts once, even when they qualify for several rules, unless a rule sets `distinct: false`.
- Optional: authors of a definition change cannot approve requests planned with that definition for N days (`separationOfDuties.definitionAuthors: 7d`).
- Approving a request that includes a plan with destroys can require a second approver (`destroys: requireTwo`).

### Delegation, reminders, escalation

- Approvers can delegate to a named colleague for a date range (out of office). The delegate's approval is recorded as "by X on behalf of Y".
- Reminders go to eligible approvers after a configurable delay; after a second delay, escalation goes to a fallback group.
- Requests expire after `expiry`; the PR is closed with a comment.

### Step-up authentication

For rules with `stepUp`, an approval is accepted only if the approver
authenticated with the IdP within the last N minutes, using OIDC `max_age` (and
`acr_values` when the IdP supports MFA levels). The UI triggers a
re-authentication before the approve button works.

### Change windows and freezes

```yaml
changeWindows:
  - name: prod-business-hours
    when: "params.env == 'prod'"
    allow: ["Mon-Thu 08:00-16:00 Europe/Zurich"]
  - name: year-end-freeze
    deny: ["2026-12-18T00:00 to 2027-01-05T00:00 Europe/Zurich"]
```

An approved request outside its window waits in *Approved* and merges when the
window opens. The request page shows the next possible merge time.

### Break-glass

For incidents, a member of the `break-glass` role can merge a request without
the normal approvals:

- they must give a reason and an incident reference;
- `quai/approval` is set to success with "break-glass by <name>";
- security and the owning team are alerted immediately;
- a post-incident review is created and stays open until two reviewers sign it off.

## ITSM

For organizations whose change management lives in ServiceNow (or Jira Service
Management):

- On submit, Quai creates a change request (standard change for catalog items marked as pre-approved; normal change otherwise) with the diff, plan summary and risk fields.
- The `external: servicenow` rule is satisfied when the change request reaches an approved state.
- On apply, Quai moves the change request to implemented and closes it, attaching the evidence bundle (see [security and compliance](09-security-and-compliance.md#evidence-bundle)).
- If the change request is rejected or cancelled, the Quai request is rejected.

## Notifications

Events: request submitted, approval needed, approved, rejected, stale, merged,
applied, failed, expiring soon. Channels: email, Microsoft Teams, Slack,
outgoing webhooks. Approvers can approve from Teams or Slack messages when the
rule does not require step-up authentication. See
[integrations](11-integrations.md).
