-- name: CreateProject :one
INSERT INTO projects (
    name,
    max_running_sandboxes,
    creation_rate_limit
) VALUES (
    sqlc.arg(name),
    sqlc.arg(max_running_sandboxes),
    sqlc.arg(creation_rate_limit)
)
RETURNING id, name, max_running_sandboxes, creation_rate_limit, created_at;

-- name: ListUserProjects :many
SELECT
    p.id,
    p.name,
    p.max_running_sandboxes,
    p.creation_rate_limit,
    p.created_at
FROM projects p
JOIN project_members pm ON pm.project_id = p.id
WHERE pm.user_id = sqlc.arg(user_id)
ORDER BY p.created_at DESC, p.id DESC
LIMIT sqlc.arg(row_limit);

-- name: ListUserProjectsAfter :many
SELECT
    p.id,
    p.name,
    p.max_running_sandboxes,
    p.creation_rate_limit,
    p.created_at
FROM projects p
JOIN project_members pm ON pm.project_id = p.id
WHERE pm.user_id = sqlc.arg(user_id)
  AND (p.created_at, p.id) < (
        sqlc.arg(cursor_created_at)::timestamptz,
        sqlc.arg(cursor_id)::uuid
    )
ORDER BY p.created_at DESC, p.id DESC
LIMIT sqlc.arg(row_limit);

-- name: GetAuthorizedProject :one
SELECT
    p.id,
    p.name,
    p.max_running_sandboxes,
    p.creation_rate_limit,
    p.created_at
FROM projects p
JOIN project_members pm ON pm.project_id = p.id
WHERE p.id = sqlc.arg(project_id)
  AND pm.user_id = sqlc.arg(user_id);

-- name: AddProjectMember :one
INSERT INTO project_members (
    project_id,
    user_id
) VALUES (
    sqlc.arg(project_id),
    sqlc.arg(user_id)
)
ON CONFLICT DO NOTHING
RETURNING project_id, user_id, created_at;

-- name: GetProjectMember :one
SELECT project_id, user_id, created_at
FROM project_members
WHERE project_id = sqlc.arg(project_id)
  AND user_id = sqlc.arg(user_id);

-- name: LockAuthorizedProject :one
SELECT
    p.id,
    p.name,
    p.max_running_sandboxes,
    p.creation_rate_limit,
    p.created_at
FROM projects p
JOIN project_members pm ON pm.project_id = p.id
WHERE p.id = sqlc.arg(project_id)
  AND pm.user_id = sqlc.arg(user_id)
FOR UPDATE OF p;
