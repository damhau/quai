# ADR 0002: Quai is the approval authority, enforced by a required check

Status: Accepted · 2026-10-26

## Context

With pull requests, two approval systems could apply: Quai's rules and the Git
provider's review (CODEOWNERS, required reviewers). Running both confuses
users and doubles the work. Provider reviews are path-based: they cannot
express "prod and large needs FinOps" or "cost above 500 CHF needs a manager".

## Decision

Quai is the single approval authority. Its decision is published as a
`quai/approval` check on the pull request, and branch protection requires it.
The check is restricted to Quai's identity where the provider allows it.
Approvals are bound to a content hash of the change set; any change to what
was reviewed voids them.

CODEOWNERS can still contribute: the `codeowners` approver kind makes the
owners of the touched paths approvers in Quai.

## Consequences

- Approval logic is expressive (inputs, plan results, cost, owners, external ITSM state) and enforced by the provider, not just by Quai's goodwill.
- Quai must verify continuously that branch protection still requires the check, and treat a missing requirement as a security incident.
- Merges without the check are detectable as bypasses.
- Customers who insist on human review in the provider can set `merge: manual` and keep their own required reviewers; Quai's check is still required.

## Alternatives considered

- **Provider review is the authority; Quai only displays it.** Rejected: path-based rules are too coarse, and the approval history would be split across providers.
- **Quai approves and bypasses branch protection with an admin token.** Rejected: unverifiable from the repository's side and a dangerous credential.
