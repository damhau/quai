-- name: LockAudit :exec
-- Serializes the audit log's writers, each in the transaction of what it records, until it
-- commits: seq has no gaps and each event chains on the last. Taken last in a transaction.
SELECT pg_advisory_xact_lock(hashtext('quai.audit'));

-- name: LastAuditEvent :one
SELECT seq, hash FROM audit_events ORDER BY seq DESC LIMIT 1;

-- name: AuditClock :one
-- The time of the events being written: the clock, not the transaction's start, so that times
-- follow seq; to the millisecond, as the events say it.
SELECT date_trunc('milliseconds', clock_timestamp())::timestamptz AS at;

-- name: InsertAuditEvent :exec
INSERT INTO audit_events (seq, at, actor, action, object, details, prev, hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: InsertAuditPersonal :exec
INSERT INTO audit_personal (hash, seq, salt, value) VALUES ($1, $2, $3, $4);

-- name: ListAuditEvents :many
-- Events newest first, before a seq, filtered: an action or its prefix (request: request.*), an
-- actor by name, an object by its ID, its resource or a name, and a time range. Names are
-- personal data: they match through audit_personal.
SELECT * FROM audit_events e
WHERE (sqlc.narg(before)::bigint IS NULL OR e.seq < sqlc.narg(before))
  AND (sqlc.narg(action)::text IS NULL OR e.action = sqlc.narg(action) OR starts_with(e.action, sqlc.narg(action) || '.'))
  AND (sqlc.narg(actor)::text IS NULL OR e.actor ->> 'id' = sqlc.narg(actor)
       OR e.actor ->> 'id' IN (SELECT p.hash FROM audit_personal p WHERE p.value = sqlc.narg(actor)))
  AND (sqlc.narg(object_type)::text IS NULL OR e.object ->> 'type' = sqlc.narg(object_type))
  AND (sqlc.narg(object)::text IS NULL OR e.object ->> 'id' = sqlc.narg(object) OR e.object ->> 'resource' = sqlc.narg(object)
       OR e.object ->> 'id' IN (SELECT p.hash FROM audit_personal p WHERE p.value = sqlc.narg(object)))
  AND (sqlc.narg(since)::timestamptz IS NULL OR e.at >= sqlc.narg(since))
  AND (sqlc.narg(until)::timestamptz IS NULL OR e.at < sqlc.narg(until))
ORDER BY e.seq DESC
LIMIT sqlc.arg(max_rows);

-- name: AuditEventsFrom :many
-- Events in seq order from a seq, for verifying the chain.
SELECT * FROM audit_events WHERE seq >= $1 ORDER BY seq LIMIT sqlc.arg(max_rows);

-- name: AuditPersonalOf :many
-- The clear personal values of events, those not pseudonymized yet.
SELECT hash, salt, value FROM audit_personal WHERE seq = ANY (sqlc.arg(seqs)::bigint[]);

-- name: LastLoginGrants :one
-- The grants a person had at their last login, to tell when they change.
SELECT e.details FROM audit_personal p JOIN audit_events e ON e.seq = p.seq
WHERE p.value = sqlc.arg(name)::text AND e.action = 'auth.login' AND e.actor ->> 'id' = p.hash
ORDER BY e.seq DESC
LIMIT 1;

-- name: AuditProtected :one
-- Whether the role the server connects as can only read and append the audit log: it can neither
-- change nor delete events or checkpoints, nor change the retention, nor become a role that could.
SELECT NOT (
    has_table_privilege('audit_events', 'UPDATE') OR has_table_privilege('audit_events', 'DELETE')
    OR has_table_privilege('audit_events', 'TRUNCATE')
    OR has_table_privilege('audit_personal', 'UPDATE') OR has_table_privilege('audit_personal', 'DELETE')
    OR has_table_privilege('audit_personal', 'TRUNCATE')
    OR has_table_privilege('audit_checkpoints', 'UPDATE') OR has_table_privilege('audit_checkpoints', 'DELETE')
    OR has_table_privilege('audit_checkpoints', 'TRUNCATE')
    OR has_table_privilege('audit_retention', 'INSERT') OR has_table_privilege('audit_retention', 'UPDATE')
    OR has_table_privilege('audit_retention', 'DELETE') OR has_table_privilege('audit_retention', 'TRUNCATE')
    OR (SELECT r.rolsuper FROM pg_catalog.pg_roles r WHERE r.rolname = current_user)
    OR pg_has_role(current_user, (SELECT c.relowner FROM pg_catalog.pg_class c WHERE c.oid = 'audit_events'::regclass), 'MEMBER')
)::boolean AS protected;

-- name: InsertAuditCheckpoint :exec
-- Another worker may have signed the same seq first: its checkpoint stays.
INSERT INTO audit_checkpoints (seq, hash, at, key, signature) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (seq) DO NOTHING;

-- name: AuditCheckpointAt :one
SELECT * FROM audit_checkpoints WHERE seq = $1;

-- name: LatestAuditCheckpoint :one
SELECT * FROM audit_checkpoints ORDER BY seq DESC LIMIT 1;

-- name: AuditCheckpoints :many
-- Every checkpoint kept, in seq order: a few thousand a year.
SELECT * FROM audit_checkpoints ORDER BY seq;

-- name: PruneAudit :one
-- Deletes what the retention lets go, up to a checkpoint (audit_prune, the schema owner's).
-- checkpoint is 0 when nothing was deleted.
SELECT coalesce(p.checkpoint, 0)::bigint AS checkpoint, p.events::bigint AS events FROM audit_prune() p;

-- name: AuditRetention :one
SELECT retention FROM audit_retention;

-- name: LastAuditVerification :one
-- The outcome of the last verification of the chain, which the worker records.
SELECT * FROM audit_events WHERE action = ANY ('{audit.verified,audit.broken}'::text[]) ORDER BY seq DESC LIMIT 1;
