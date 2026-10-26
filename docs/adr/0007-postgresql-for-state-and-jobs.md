# ADR 0007: PostgreSQL for state and jobs

Status: Accepted · 2026-10-27

## Context

The prototype uses SQLite in one process and runs Git operations inline in
HTTP requests. Enterprise installs need several replicas, durable background
work that survives restarts, retries, schedules, and serialized work per
repository.

## Decision

PostgreSQL is the only stateful dependency. Job queueing uses River, which
stores jobs in PostgreSQL. A state change and the jobs it triggers are
committed in the same transaction. Per-repository serialization uses
PostgreSQL advisory locks; jobs that cannot get the lock snooze and retry.

## Consequences

- One database to operate, back up and secure; no Redis or message broker.
- Exactly-once state transitions with at-least-once jobs: every job must be idempotent.
- Throughput is bounded by PostgreSQL, comfortably above the targets in [deployment and operations](../operations/deployment-and-operations.md#scale-targets).

## Alternatives considered

- **Redis-based queues (Asynq) or NATS.** Rejected: a second stateful system, and enqueue would not be transactional with state changes.
- **Temporal.** Powerful for long workflows, but a heavy additional platform to run on-premises. Revisit if change-set orchestration grows complex.
