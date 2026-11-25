# Sandbox API

Go 1.27.4 API for authenticated projects and sandboxes.

This file is the local setup and the reviewer walkthrough.

## Project Overview

The API stores users, projects, memberships, and sandbox lifecycle records. A sandbox row is `running` or `stopped`. PostgreSQL is authoritative for those records and for running occupancy. Redis holds short-lived OAuth state and the per-project creation window. Hydra issues access tokens. The API verifies those tokens and does not collect passwords.

Every member of a project can create sandboxes, list them, and stop them. A caller who is not a member receives the same `404 NOT_FOUND` as a caller who names a missing resource. These operations do not return `403`.

## Technology Stack

- Go 1.27.4, with pgx for PostgreSQL and sqlc for query code.
- Goose v3.30.1 for schema migrations.
- PostgreSQL 16 and Redis 7, started by Docker Compose.
- Ory Hydra v2.2.0, with the login and consent UI in `dev/hydra-login`.

## Repository Structure

- `cmd/api`: process entrypoint.
- `internal/auth`: access-token checks.
- `internal/config`: environment loading.
- `internal/httpapi`: HTTP routes and responses.
- `internal/oauth`: login flow.
- `internal/postgres`: database outage classification.
- `internal/projects`: project and membership operations.
- `internal/sandboxes`: sandbox operations.
- `internal/ratelimit`: creation admission.
- `internal/db/sqlc`: generated query package.
- `db/migrations`: Goose schema.
- `db/queries`: SQL compiled by sqlc.
- `dev/hydra-login`: local Hydra login and consent image.
- Repository root: `sqlc.yaml`, `docker-compose.yml`, `.env.example`, `go.mod`, and `go.sum`.

## Prerequisites

Install Docker and Go 1.27.4. Leave these host ports free: 5432, 6379, 4444, 4445, 3000, and 8080.

## Local Execution

Export the fixture environment before the migration and the API process. The API reads the process environment. It does not open a dotenv file.

```sh
docker compose up -d --build

set -a
source .env.example
set +a

go install github.com/pressly/goose/v3/cmd/goose@v3.30.1
export PATH="$(go env GOPATH)/bin:$PATH"
goose -dir db/migrations postgres "$DATABASE_URL" up

go tool sqlc generate

go run ./cmd/api
```

Then open `http://localhost:8080/auth/login` in a browser. The sections below explain each command.

## Docker Compose Startup

```sh
docker compose up -d --build
```

`--build` builds the login UI from `dev/hydra-login`. Compose starts:

| Service | Address |
| --- | --- |
| PostgreSQL 16 | `localhost:5432` |
| Redis 7 | `localhost:6379` |
| Hydra public | `http://localhost:4444` |
| Hydra admin | `http://localhost:4445` |
| Login and consent UI | `http://localhost:3000` |

PostgreSQL data stays in the `postgres-data` volume. Hydra keeps its client and sessions in memory. After a Hydra restart, recreate the client:

```sh
docker compose up hydra-bootstrap
```

A repeated bootstrap updates the existing client. The client id is `e2b-assignment`, the secret is `e2b-assignment-secret`, and the redirect URI is `http://localhost:8080/auth/callback`.

## Environment Configuration

Load the fixture values into the shell:

```sh
set -a
source .env.example
set +a
```

`.env` is gitignored. `.env.example` is the local fixture. Every variable is required at startup.

| Variable | Fixture Value | Role |
| --- | --- | --- |
| `HTTP_ADDR` | `:8080` | Listen address |
| `DATABASE_URL` | `postgres://e2b:e2b@localhost:5432/e2b` | PostgreSQL connection |
| `REDIS_ADDR` | `localhost:6379` | Redis address |
| `OAUTH_ISSUER` | `http://localhost:4444` | Expected token issuer |
| `OAUTH_CLIENT_ID` | `e2b-assignment` | Hydra client id |
| `OAUTH_CLIENT_SECRET` | `e2b-assignment-secret` | Hydra client secret |
| `OAUTH_REDIRECT_URI` | `http://localhost:8080/auth/callback` | Authorization-code redirect |
| `OAUTH_AUDIENCE` | `urn:e2b:sandbox-api` | Required access-token audience |
| `DEFAULT_MAX_RUNNING_SANDBOXES` | `10` | Running quota copied onto each new project |
| `DEFAULT_CREATION_RATE_LIMIT` | `20` | Creation allowance copied onto each new project |
| `RATE_LIMIT_WINDOW` | `60s` | Length of each creation window |
| `OAUTH_STATE_TTL` | `5m` | Lifetime of one login transaction |
| `REQUEST_TIMEOUT` | `5s` | Per-request deadline |

Changing the two default limits affects projects created afterward. Existing projects keep the limits stored on their rows.

The issuer, client id, and audience also determine the Hydra URLs. Authorization is `{OAUTH_ISSUER}/oauth2/auth`, the token endpoint is `{OAUTH_ISSUER}/oauth2/token`, and signing keys are `{OAUTH_ISSUER}/.well-known/jwks.json`.

## Database Migrations

Apply the schema after `DATABASE_URL` is exported. The API process does not migrate on startup.

`go.mod` requires Goose v3.30.1. Install that command, then put its directory on `PATH`. The default GOPATH puts the binary at `~/go/bin`.

```sh
go install github.com/pressly/goose/v3/cmd/goose@v3.30.1
export PATH="$(go env GOPATH)/bin:$PATH"
goose -dir db/migrations postgres "$DATABASE_URL" up
```

Install the command this way. `go run` of the Goose CLI inside this module stops on driver packages that are not listed in `go.sum`.

The migration creates `users`, `projects`, `project_members`, and `sandboxes`. `users` is unique on issuer and subject. A running sandbox has a null `stopped_at`. A stopped sandbox has `stopped_at` at or after `created_at`.

## sqlc Code Generation

From the repository root:

```sh
go tool sqlc generate
```

That runs sqlc v1.31.1 from `go.mod`, using `sqlc.yaml`. sqlc reads `db/queries` and `db/migrations`. It does not connect to PostgreSQL. Generated Go is written to `internal/db/sqlc`, and that package is already present in the tree. Application services use it for PostgreSQL.

## Go Application Startup

```sh
go run ./cmd/api
```

The process listens on `:8080` after PostgreSQL and Redis accept a connection. One Redis client stores OAuth state. The other admits sandbox creation. Stop the process with SIGINT or SIGTERM. It drains open requests, then closes the pools.

## Hydra Authentication and Access-Token Acquisition

Open this URL in a browser:

```text
http://localhost:8080/auth/login
```

`GET /auth/login` stores a login transaction in Redis, sets the `sandbox_oauth_binding` cookie, and redirects to Hydra. Hydra sends the browser to `http://localhost:3000/login` and then to `http://localhost:3000/consent`. The callback returns JSON and sets `Cache-Control: no-store`.

```json
{
  "access_token": "<hydra-issued-access-token>",
  "token_type": "Bearer",
  "expires_in": 3600,
  "user_id": "11111111-1111-4111-8111-111111111111"
}
```

`expires_in` is present when Hydra sends it. Copy `access_token` and `user_id` from that page. Later `/v1` calls send:

```text
Authorization: Bearer <access_token>
```

The binding cookie is why login starts in a browser, and why a stolen code and state cannot be finished by a client that never received the cookie. A bare request to `/auth/login` from `curl` does not complete the consent screen. Use a second browser profile for the second account so each login keeps its own cookie and Hydra session.

Identity is the OAuth issuer and subject. A verified email is stored when Hydra marks it verified. Email is not a lookup key. The callback body does not include an ID token or a refresh token, and the user row stores neither token nor a password.

The API accepts an RS256 access token whose audience is `urn:e2b:sandbox-api`. It rejects an ID token, whose audience is `e2b-assignment`. The first successful callback for an issuer and subject inserts the user. A later login returns the same `user_id`. Login state is single use, so a second callback with the same state fails. A subject that has never completed callback is `401` on `/v1`, and `/v1` does not insert users.

Development accounts use password `foobar`. The OAuth subject is the email address.

| Email | Subject | Walkthrough Role |
| --- | --- | --- |
| `foo@bar.com` | `foo@bar.com` | User A |
| `bar@foo.com` | `bar@foo.com` | User B |
| `baz@foo.com` | `baz@foo.com` | User C, when a third registered user is required |

Replace the shell placeholders below with the values copied from each callback:

```sh
USER_A_TOKEN='<access token for foo@bar.com>'
USER_A_ID='<user_id for foo@bar.com>'
USER_B_TOKEN='<access token for bar@foo.com>'
USER_B_ID='<user_id for bar@foo.com>'
```

## Reviewer Walkthrough

Run the following sections in order. User A creates the project and the first sandbox. User B is refused until User A adds that membership. Afterward both members can list and stop. Resource ids in the examples are the ids returned by earlier responses.

### Project Creation

User A creates a project. The name is trimmed and then must be 1 to 255 Unicode characters. Names may repeat.

```sh
curl -sS -D - -X POST http://localhost:8080/v1/projects \
  -H "Authorization: Bearer $USER_A_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"Agent Development"}'
```

The status is `201 Created`.

```json
{
  "id": "6d502949-7e8c-4f47-8b0c-3f1e3b913001",
  "name": "Agent Development",
  "created_at": "2026-11-24T20:00:00Z"
}
```

```sh
PROJECT_ID='<id from the response>'
```

The creator is a member. There is one membership level, and the schema has no role column. The response does not include the membership row. The project stores the current default running quota and creation allowance.

### Sandbox Creation

Sandbox creation accepts an empty body.

```sh
curl -sS -D - -X POST "http://localhost:8080/v1/projects/$PROJECT_ID/sandboxes" \
  -H "Authorization: Bearer $USER_A_TOKEN"
```

The status is `201 Created`. `status` is `running` and `stopped_at` is null.

```json
{
  "id": "7719ac21-1b89-4865-89b5-0ed2f8721003",
  "project_id": "6d502949-7e8c-4f47-8b0c-3f1e3b913001",
  "status": "running",
  "created_at": "2026-11-24T20:10:00Z",
  "stopped_at": null
}
```

```sh
SANDBOX_ID='<id from the response>'
```

Each committed create inserts a distinct sandbox. Sending the request again creates another row.

### Access before Membership

User B's project list omits Project A:

```sh
curl -sS http://localhost:8080/v1/projects \
  -H "Authorization: Bearer $USER_B_TOKEN"
```

The sandbox list and the stop:

```sh
curl -sS -D - "http://localhost:8080/v1/projects/$PROJECT_ID/sandboxes" \
  -H "Authorization: Bearer $USER_B_TOKEN"

curl -sS -D - -X DELETE "http://localhost:8080/v1/sandboxes/$SANDBOX_ID" \
  -H "Authorization: Bearer $USER_B_TOKEN"
```

Both return `404 NOT_FOUND`.

### Adding Another Registered User

User A adds User B with the `user_id` from User B's callback.

```sh
curl -sS -D - -X POST "http://localhost:8080/v1/projects/$PROJECT_ID/members" \
  -H "Authorization: Bearer $USER_A_TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"user_id\":\"$USER_B_ID\"}"
```

The first insert is `201 Created`. Repeat the same request. The status is `200 OK`, and the membership `created_at` is unchanged.

```json
{
  "project_id": "6d502949-7e8c-4f47-8b0c-3f1e3b913001",
  "user_id": "de498973-c030-4fc2-b075-4b5dbb421002",
  "created_at": "2026-11-24T20:05:00Z"
}
```

A target `user_id` that has never logged in is `404 NOT_FOUND`. User B cannot add members until User B is already a member.

### Accessible Project Listing

After the membership commit, User B sees Project A:

```sh
curl -sS http://localhost:8080/v1/projects \
  -H "Authorization: Bearer $USER_B_TOKEN"
```

```json
{
  "items": [
    {
      "id": "6d502949-7e8c-4f47-8b0c-3f1e3b913001",
      "name": "Agent Development",
      "created_at": "2026-11-24T20:00:00Z"
    }
  ],
  "next_cursor": null
}
```

The list contains projects for the authenticated user.

### Sandbox Listing

Either member can list running and stopped sandboxes:

```sh
curl -sS "http://localhost:8080/v1/projects/$PROJECT_ID/sandboxes" \
  -H "Authorization: Bearer $USER_B_TOKEN"
```

The page shape is `items` plus `next_cursor`. A running sandbox has `stopped_at` set to null. Stopped rows remain in this list.

### Sandbox Termination

User B stops the sandbox. The status is `204 No Content` and the body is empty.

```sh
curl -sS -D - -X DELETE "http://localhost:8080/v1/sandboxes/$SANDBOX_ID" \
  -H "Authorization: Bearer $USER_B_TOKEN"
```

List the sandbox again and copy `stopped_at`. Repeat the same DELETE. The status is `204` again. List once more. `status` stays `stopped`, and `stopped_at` is the timestamp from the first stop. The repeated stop releases no extra quota.

### Third Identity

`baz@foo.com` is User C. Sign in with a third browser profile when a scenario needs another registered user. Until someone adds User C to Project A, that project stays off User C's list. User C can create a separate project and receive `201`.

## Pagination

`GET /v1/projects` and `GET /v1/projects/{id}/sandboxes` accept `limit` and `cursor`. The default limit is 50. The minimum is 1 and the maximum is 100. A missing cursor starts at the newest row. Order is `created_at DESC, id DESC`.

```sh
curl -sS "http://localhost:8080/v1/projects?limit=1" \
  -H "Authorization: Bearer $USER_A_TOKEN"
```

When another row exists, `next_cursor` is a string. Pass that string back unchanged, URL-encoded as a query value:

```sh
curl -sS --get "http://localhost:8080/v1/projects" \
  -H "Authorization: Bearer $USER_A_TOKEN" \
  --data-urlencode "limit=1" \
  --data-urlencode "cursor=$NEXT_CURSOR"
```

The same parameters work on the sandbox list. A project cursor is valid only for the authenticated user's project list. A sandbox cursor is valid only for that project. A cursor from the other collection, another user, or another project is `400 INVALID_REQUEST`. An empty page has `"items": []` and `"next_cursor": null`.

A cursor is a position in the current order, not a frozen snapshot. A commit between two page reads can change what the next cursor returns.

## Creation-Rate Limits

```mermaid
flowchart TD
  auth[Access Token] --> member[Membership]
  member --> admit[Redis Window]
  admit -->|full| limited[429 Rate Limit]
  admit -->|open| lock[Lock Project]
  lock --> count[Count Running]
  count -->|full| quota[429 Quota]
  count -->|room| insert[Insert Running]
```

Sandbox creation uses one fixed window per project. The default allowance is 20 admitted attempts. The default window is 60 seconds. Members of a project share that allowance. Distinct projects have separate allowances.

A full window returns `429` with code `RATE_LIMIT_EXCEEDED` and a `Retry-After` header, in seconds. Listing and termination do not use this window, and they continue when Redis is unavailable.

Creation fails closed when Redis cannot admit the attempt, and a lost admission reply is not retried. A fixed window can admit a burst at the boundary between two windows.

To see rate exhaustion while running occupancy is still below 10:

- Use a new project.
- Admit 20 creates that do not remain running: create one sandbox, stop it, and repeat that pair.
- Finish those admissions inside the same 60-second window. A scripted loop is the practical way to do that.
- The next create returns `429 RATE_LIMIT_EXCEEDED` and `Retry-After`.
- Stopping a sandbox does not reset the window.
- After the window expires, a create is admitted again.

## Running Sandbox Quotas

Each new project stores a running quota. The fixture default is 10. Occupancy is the number of committed rows with status `running`.

`429` with code `QUOTA_EXCEEDED` means occupancy is already at that quota. This response has no `Retry-After` header.

To see quota exhaustion while the creation window still has room:

- Use a new project and create 10 sandboxes. All ten return `201`.
- The eleventh returns `429 QUOTA_EXCEEDED`.
- That eleventh attempt is still an admitted rate unit, so the window count advances even though no row was inserted.
- With the fixture defaults the count is then 11, which is below 20, so the refusal is the quota.

Window expiration does not release running occupancy. A later create on a full project still returns `QUOTA_EXCEEDED` after the window has rolled.

Stopping one running sandbox releases one slot. A following create in an open window returns `201`.

## Final-Slot Concurrency

Use a project with 9 running sandboxes and at least two unused creation attempts. Send two creates for that project at the same time:

```sh
curl -sS -D - -o /tmp/create-a.json -X POST "http://localhost:8080/v1/projects/$PROJECT_ID/sandboxes" \
  -H "Authorization: Bearer $USER_A_TOKEN" &
curl -sS -D - -o /tmp/create-b.json -X POST "http://localhost:8080/v1/projects/$PROJECT_ID/sandboxes" \
  -H "Authorization: Bearer $USER_A_TOKEN" &
wait
```

Both curls use User A so they succeed on a project User A owns.

Both requests can pass the rate check. PostgreSQL locks the project row and counts running rows for one request at a time. There is no occupancy counter column. One commit returns `201` and leaves 10 running rows. The other returns `429 QUOTA_EXCEEDED` and inserts nothing. Redis has already counted both attempts. Writes for different projects proceed independently.

## Structured Errors

Failure bodies use this object. Messages are fixed. SQL text, stack traces, and provider secrets are omitted.

```json
{
  "error": {
    "code": "INVALID_REQUEST",
    "message": "The supplied request is invalid."
  }
}
```

| HTTP Status | Code | When Used |
| --- | --- | --- |
| 400 | `INVALID_REQUEST` | Invalid parameters, payload, or pagination. Message: `The supplied request is invalid.` |
| 400 | `INVALID_OAUTH_STATE` | Missing, incorrect, expired, mismatched, or consumed OAuth state. Message: `The OAuth state is invalid.` |
| 400 | `OAUTH_AUTHORIZATION_FAILED` | Consent denial, or a missing or rejected authorization code. Message: `OAuth authorization failed.` |
| 401 | `UNAUTHENTICATED` | Missing, ambiguous, or invalid authentication. Message: `A valid access token is required.` |
| 404 | `NOT_FOUND` | Unknown route, missing resource, or a resource the caller cannot access. Message: `The requested resource was not found.` |
| 405 | `METHOD_NOT_ALLOWED` | Method the route does not serve. Message: `The requested method is not allowed.` |
| 413 | `REQUEST_TOO_LARGE` | Body above 16 KiB. Message: `The request body is too large.` |
| 415 | `UNSUPPORTED_MEDIA_TYPE` | Non-empty body whose content type is not JSON. Message: `The request content type is not supported.` |
| 429 | `RATE_LIMIT_EXCEEDED` | Creation window is full. Includes `Retry-After`. Message: `The project has reached its sandbox creation request limit.` |
| 429 | `QUOTA_EXCEEDED` | Running occupancy is at the stored quota. No `Retry-After`. Message: `The project has reached its running sandbox limit.` |
| 500 | `INTERNAL_ERROR` | Unexpected application failure. Message: `The request could not be completed.` |
| 502 | `OAUTH_PROVIDER_ERROR` | Invalid or unexpected response from Hydra. Message: `The OAuth provider returned an invalid response.` |
| 503 | `SERVICE_UNAVAILABLE` | PostgreSQL, Redis, or the signing-key endpoint cannot complete a required step. Message: `A required service is unavailable.` |

A missing `Authorization` header returns `401` and `WWW-Authenticate: Bearer`. A token that fails verification returns `401` and `WWW-Authenticate: Bearer error="invalid_token"`. A wrong method returns `405` and an `Allow` header listing the methods that route serves.

JSON object bodies use `Content-Type: application/json`. Unknown fields, a second JSON value, a malformed UUID, and a `limit` outside 1 through 100 are `400 INVALID_REQUEST`. Sandbox creation is the route that accepts an empty body. Invalid creation input consumes no rate allowance.

## Future Scalability

These paths are not built. A later change still has to preserve membership checks, quota correctness, and the committed lifecycle.

| Concern | Current Implementation | Potential Evolution |
| --- | --- | --- |
| API capacity | Stateless Go service | Horizontal replication |
| Database connections | pgx connection pool | External connection pooling |
| Historical sandbox growth | Indexed PostgreSQL queries | Partitioning and archival |
| Very large datasets | Project-scoped queries | Tenant-oriented sharding |
| High active quota | Indexed running count | Transactional occupancy counter |
| Hot projects | Project-level serialization | Optimize transaction duration and capacity coordination |
| Large membership sets | Indexed membership joins | Further query and index optimization |
| Rate-limit bursts | Fixed window | Token bucket or sliding window |
| Read scaling | Authoritative PostgreSQL | Carefully selected replica reads |

Replica reads would be limited to responses that can tolerate lag. Quota and lifecycle commits would stay on the primary. An occupancy counter would move inside the same locked transaction as the row change. A different rate algorithm would still fail closed when the store cannot decide.

## Development Fixture Limitations

- Redis data loss resets login transactions and creation windows. Users, projects, memberships, sandboxes, and running occupancy remain in PostgreSQL.
- Access tokens are not revoked before expiry.
- Refresh, logout, account recovery, invitations, rename, project deletion, and sandbox compute have no routes.
- An uncertain create is not replayed. Replaying a lost create can insert another row.
- This stack states no production throughput.
- Sandbox rows record lifecycle only. The service provisions no machines, networks, or processes for them.
