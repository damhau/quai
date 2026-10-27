-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE expiry < now();
