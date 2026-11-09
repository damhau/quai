-- name: PutDefinitionSet :execrows
INSERT INTO definition_sets (repository, commit, active, problems) VALUES ($1, $2, $3, $4)
ON CONFLICT (org_id, repository, commit) DO NOTHING;

-- name: GetDefinitionSet :one
SELECT * FROM definition_sets WHERE repository = $1 AND commit = $2;

-- name: LatestDefinitionSet :one
-- The set stored last for the repository, at another commit than the one given.
SELECT * FROM definition_sets WHERE repository = $1 AND commit <> $2 ORDER BY created_at DESC LIMIT 1;

-- name: DefinitionsSince :one
-- When a period of seconds ending now began, by the database's clock, which stamped the sets, and
-- the commit of the set stored last for the repository by then: the head Quai had then, "" when it
-- had none (separation of duties, docs/design/05-delivery-and-approvals.md#separation-of-duties).
SELECT w.since::timestamptz AS since, COALESCE(d.commit, '')::text AS commit
FROM (SELECT now() - make_interval(secs => sqlc.arg(seconds)::float8) AS since) w
LEFT JOIN LATERAL (
    SELECT commit FROM definition_sets
    WHERE repository = sqlc.arg(repository) AND created_at <= w.since
    ORDER BY created_at DESC LIMIT 1
) d ON true;
