-- name: PutDefinitionSet :execrows
INSERT INTO definition_sets (repository, commit, active, problems) VALUES ($1, $2, $3, $4)
ON CONFLICT (org_id, repository, commit) DO NOTHING;

-- name: GetDefinitionSet :one
SELECT * FROM definition_sets WHERE repository = $1 AND commit = $2;

-- name: LatestDefinitionSet :one
-- The set stored last for the repository, at another commit than the one given.
SELECT * FROM definition_sets WHERE repository = $1 AND commit <> $2 ORDER BY created_at DESC LIMIT 1;
