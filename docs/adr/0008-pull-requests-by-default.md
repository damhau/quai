# ADR 0008: Pull requests by default

Status: Accepted · 2026-10-26

## Context

The prototype commits directly to the target branch after approval. That means
nobody sees a Terraform plan before approving, and the customer's CI checks
(validation, policy, cost) cannot run before the change lands.

## Decision

The default delivery mode is a pull request opened at submit time. CI plans
while approvals are pending; Quai shows the plan, policy and cost results to
approvers; Quai merges once approvals and checks are green. Direct commits
remain available per repository for sandboxes, demos and providers without an
API.

## Consequences

- Approvers decide with the plan in front of them.
- Approval rules can use plan results (destroys, cost).
- More moving parts: branches, staleness, rebuilds, merge queues ([delivery and approvals](../design/05-delivery-and-approvals.md)).
- Lead time includes CI duration.

## Alternatives considered

- **Direct commits only.** Rejected for production: no plan before approval, no CI gate.
- **Open the PR only after approval.** Rejected: approvers would approve without the plan, and CI would run after the decision.
