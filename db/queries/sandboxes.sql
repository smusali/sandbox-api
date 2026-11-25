-- name: CountRunningSandboxes :one
SELECT COUNT(*)
FROM sandboxes
WHERE project_id = sqlc.arg(project_id)
  AND status = 'running';

-- name: CreateSandbox :one
INSERT INTO sandboxes (
    project_id,
    status
) VALUES (
    sqlc.arg(project_id),
    'running'
)
RETURNING id, project_id, status, created_at, stopped_at;

-- name: ListProjectSandboxes :many
SELECT id, project_id, status, created_at, stopped_at
FROM sandboxes
WHERE project_id = sqlc.arg(project_id)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit);

-- name: ListProjectSandboxesAfter :many
SELECT id, project_id, status, created_at, stopped_at
FROM sandboxes
WHERE project_id = sqlc.arg(project_id)
  AND (created_at, id) < (
        sqlc.arg(cursor_created_at)::timestamptz,
        sqlc.arg(cursor_id)::uuid
    )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit);

-- name: GetAuthorizedSandbox :one
SELECT
    s.id,
    s.project_id,
    s.status,
    s.created_at,
    s.stopped_at
FROM sandboxes s
JOIN project_members pm ON pm.project_id = s.project_id
WHERE s.id = sqlc.arg(sandbox_id)
  AND pm.user_id = sqlc.arg(user_id);

-- name: StopRunningSandbox :execrows
UPDATE sandboxes
SET status = 'stopped',
    stopped_at = GREATEST(clock_timestamp(), created_at)
WHERE id = sqlc.arg(id)
  AND project_id = sqlc.arg(project_id)
  AND status = 'running';
