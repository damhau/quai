# ADR 0001: Git is the system of record

Status: Accepted · 2026-10-26

## Context

vRA-style portals keep their own record of what was deployed and drive the
infrastructure directly. Over time the portal's view and reality diverge, and
the portal becomes a second source of truth nobody fully trusts. The
organizations Quai targets already manage infrastructure as code in Git.

## Decision

Desired state lives only in the customer's Git repositories. Quai changes
infrastructure for declarative operations exclusively by committing to those
repositories; the customer's pipelines apply. Quai's database holds
operational state (requests, approvals, runs, audit) and caches that can be
rebuilt from Git, except the audit log.

## Consequences

- Quai needs no cloud or hypervisor credentials for declarative changes, only Git credentials.
- Humans, bots and Quai can all change the same repository; Quai must read what others wrote (inventory from Git, minimal-diff editing).
- Status depends on runners reporting back; without a runner, Quai knows only what is in Git.
- Imperative operations (reboot, snapshot) are the explicit exception, handled by executors and audited ([day-2 actions](../design/07-day2-actions-and-executors.md)).

## Alternatives considered

- **Quai as the system of record, pushing to Git as an export.** Rejected: recreates vRA's divergence problem.
- **Quai calling Terraform or cloud APIs directly.** Rejected: Quai would own the engine, its credentials and its state, competing with the customer's existing pipelines instead of fronting them.
