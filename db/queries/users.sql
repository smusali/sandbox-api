-- name: UpsertUser :one
INSERT INTO users (
    oauth_issuer,
    oauth_subject,
    email
) VALUES (
    sqlc.arg(oauth_issuer),
    sqlc.arg(oauth_subject),
    sqlc.arg(email)
)
ON CONFLICT (oauth_issuer, oauth_subject) DO UPDATE
SET email = COALESCE(EXCLUDED.email, users.email)
RETURNING id, oauth_issuer, oauth_subject, email, created_at;

-- name: GetUserByIdentity :one
SELECT id, oauth_issuer, oauth_subject, email, created_at
FROM users
WHERE oauth_issuer = sqlc.arg(oauth_issuer)
  AND oauth_subject = sqlc.arg(oauth_subject);

-- name: GetUserByID :one
SELECT id, oauth_issuer, oauth_subject, email, created_at
FROM users
WHERE id = sqlc.arg(id);
