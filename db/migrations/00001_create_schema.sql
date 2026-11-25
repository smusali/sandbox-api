-- +goose Up
CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    oauth_issuer TEXT NOT NULL,
    oauth_subject TEXT NOT NULL,
    email TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT users_external_identity_unique
        UNIQUE (oauth_issuer, oauth_subject),

    CONSTRAINT users_issuer_not_empty
        CHECK (length(oauth_issuer) > 0),

    CONSTRAINT users_subject_not_empty
        CHECK (length(oauth_subject) > 0)
);

CREATE TABLE projects (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(255) NOT NULL,
    max_running_sandboxes INTEGER NOT NULL,
    creation_rate_limit INTEGER NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT projects_name_not_empty
        CHECK (length(trim(name)) > 0),

    CONSTRAINT projects_running_limit_positive
        CHECK (max_running_sandboxes > 0),

    CONSTRAINT projects_creation_limit_positive
        CHECK (creation_rate_limit > 0)
);

CREATE TABLE project_members (
    project_id UUID NOT NULL REFERENCES projects(id),
    user_id UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (project_id, user_id)
);

CREATE TABLE sandboxes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID NOT NULL REFERENCES projects(id),
    status TEXT NOT NULL DEFAULT 'running',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    stopped_at TIMESTAMPTZ,

    CONSTRAINT sandboxes_valid_status
        CHECK (status IN ('running', 'stopped')),

    CONSTRAINT sandboxes_valid_lifecycle
        CHECK (
            (status = 'running' AND stopped_at IS NULL)
            OR (
                status = 'stopped'
                AND stopped_at IS NOT NULL
                AND stopped_at >= created_at
            )
        )
);

CREATE INDEX idx_project_members_user_project
    ON project_members (user_id, project_id);

CREATE INDEX idx_sandboxes_project_created
    ON sandboxes (
        project_id,
        created_at DESC,
        id DESC
    );

CREATE INDEX idx_sandboxes_running_project
    ON sandboxes (project_id)
    WHERE status = 'running';

-- +goose Down
DROP TABLE sandboxes;
DROP TABLE project_members;
DROP TABLE projects;
DROP TABLE users;
