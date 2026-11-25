# Solution

This document describes the architecture that is implemented. `CASES.md` is the behavioral contract. `README.md` is the local setup and the reviewer walkthrough. The scalability paths at the end are recorded only. They are not built.

## Problem interpretation

Callers authenticate with Hydra, then keep durable records of projects, memberships, and sandbox lifecycle. A sandbox is a row with status `running` or `stopped`. The service answers the eight HTTP operations in `CASES.md`: login, callback, project create, project list, add member, sandbox create, sandbox list, and sandbox stop.

PostgreSQL holds users, projects, memberships, sandbox rows, and the running occupancy those rows imply. Redis holds login transactions and the temporary creation allowance. Hydra issues tokens. The API is an OAuth client and a resource server.

## Scope and assumptions

`CASES.md` fixes the observable contract: project membership as the tenancy boundary, equal permissions for every member, server-generated UUIDs, a persisted running quota of 10 and a persisted creation allowance of 20 on each new project, a 60-second fixed creation window, keyset pagination, `404` for hidden resources, and non-idempotent project and sandbox creation.

This implementation adds the following choices around that contract:

- Identity is the OAuth issuer and subject. A verified email is optional profile data and is stored when Hydra marks it verified. An unverified email is omitted. Email is not a lookup key.
- Project names are trimmed display text, 1 to 255 Unicode characters, and they may repeat.
- The process defaults for quota and creation allowance are copied onto the project row at insert. Later changes to those environment values leave existing rows alone.
- There is one membership level. The schema has no role column.
- Refresh, logout, account recovery, invitations, rename, project deletion, and sandbox compute have no routes.
- Quota correctness uses `READ COMMITTED` and a project-row lock. The running count is a query under that lock. There is no occupancy counter column.
- Creation admission uses a Redis client with command retries disabled, so a lost reply is not applied a second time.
- List cursors are an internal versioned document. Clients pass the token through unchanged.
- Signing keys stay cached for 15 minutes, retrievals are at least 30 seconds apart, and a cached key remains usable for 24 hours after the last successful retrieval.

## Component responsibilities

`cmd/api` loads configuration, opens the PostgreSQL pool and two Redis clients, builds the verifier, OAuth flow, project service, and sandbox service, and serves HTTP. SIGINT and SIGTERM stop the listener and drain in-flight requests.

`internal/config` reads the process environment and refuses to start when a required value is missing or invalid. Authorization, token, and JWKS URLs are derived from `OAUTH_ISSUER`.

`internal/oauth` starts the authorization-code flow, stores the login transaction, exchanges the code, verifies the access token, and upserts the user.

`internal/auth` verifies RS256 access tokens against the Hydra JWKS and exposes the authenticated application user to handlers.

`internal/httpapi` is the router, the JSON and pagination validation, and the public error mapping. Handlers call the domain services. They do not embed SQL.

`internal/projects` creates a project with its creator membership, lists a user's projects, and adds a member.

`internal/sandboxes` admits creation, enforces the running quota, lists sandbox history, and stops a sandbox.

`internal/ratelimit` runs the fixed-window admission script.

`internal/postgres` recognizes a PostgreSQL outage so handlers can return `503`.

`internal/db/sqlc` is the only database API used by the application services. The queries live in `db/queries` and are compiled with the schema in `db/migrations`.

## Database model

One Goose migration creates four tables.

`users` has a server-generated UUID primary key, a required issuer, a required subject, an optional email, and `created_at`. The pair `(oauth_issuer, oauth_subject)` is unique. Both identity strings must be non-empty.

`projects` has a UUID primary key, a non-empty trimmed `name`, a positive `max_running_sandboxes`, a positive `creation_rate_limit`, and `created_at`.

`project_members` references one project and one user. The primary key is `(project_id, user_id)`. `idx_project_members_user_project` supports the membership lookups used by lists and authorization.

`sandboxes` references one project. `status` is `running` or `stopped`. A running row has a null `stopped_at`. A stopped row has `stopped_at` at or after `created_at`. `idx_sandboxes_project_created` matches the list order `(project_id, created_at DESC, id DESC)`. `idx_sandboxes_running_project` is a partial index on `project_id` for rows whose status is `running`.

## OAuth authentication decisions

`GET /auth/login` and `GET /auth/callback` are public. Every `/v1` route requires a bearer access token and a user row.

Login generates an S256 PKCE verifier and a browser binding, stores their hashes and the verifier in Redis under `sandbox-api:oauth:state:{state}`, and sets the `sandbox_oauth_binding` cookie on path `/auth/callback`. The state lifetime is `OAUTH_STATE_TTL`, five minutes in the fixture. The browser is redirected to Hydra with the PKCE challenge and the configured audience.

The callback checks the state and the cookie, then consumes the Redis record with a Lua script. The script returns the verifier only when the binding matches, and it deletes the key. A second callback with the same state fails. The authorization code is exchanged at Hydra's token endpoint. The access token is verified. `UpsertUser` inserts the issuer, subject, and optional verified email, or returns the existing user. A later login with a verified email stores that email. A login without a verified email leaves the stored email in place.

The callback JSON contains `access_token`, `token_type`, the application `user_id`, and `expires_in` when Hydra provided it. `Cache-Control` is `no-store`. The user row does not store access tokens, refresh tokens, ID tokens, or passwords. Hydra collects the password on the login UI.

Redis failure while saving or consuming state returns `503`. The API mints no token of its own.

## JWT token-purpose validation

Protected routes take one `Authorization: Bearer` header. The verifier accepts RS256, a header type of `JWT` or `at+JWT` when a type is present, and a key from the configured JWKS. Embedded keys, `jku`, `x5u`, and `crit` are rejected. Redirects while fetching keys are refused.

Claims must include an expiry. Validation allows one minute of clock skew and requires the configured issuer and the configured audience, `urn:e2b:sandbox-api` in the fixture. A token whose audience also contains the OAuth client id is rejected. That is how an ID token, whose audience is `e2b-assignment`, is kept off `/v1`. The subject must be non-empty.

A verified token is then resolved with `GetUserByIdentity`. A subject that has never completed callback is `401`. `/v1` does not insert users.

A known signing key is used for 15 minutes without contacting Hydra. An unknown key triggers one shared JWKS retrieval. Further retrievals wait out a 30-second cooldown and may use a key cached for up to 24 hours. When no cached key can verify the token and Hydra cannot be reached, the result is `503`. A present token that fails verification is `401` with `WWW-Authenticate: Bearer error="invalid_token"`. A missing credential uses `WWW-Authenticate: Bearer`.

## Tenant isolation

Project and sandbox access join `project_members`. `GetAuthorizedProject` returns a project only when the caller is a member. `GetAuthorizedSandbox` returns a sandbox only when the caller is a member of the sandbox's stored `project_id`. The client does not supply that project id on stop.

A missing row and a row the caller cannot see both become `ErrNotFound`, then `404 NOT_FOUND`, with one message. Membership addition checks the actor's membership before it looks up the target user, so a non-member learns nothing about whether the target id exists.

List queries are scoped the same way. Project pages select through the caller's memberships. Sandbox pages require membership, then read that project's rows.

## Redis rate-limiting algorithm

Sandbox creation is the only rate-limited operation. The allowance is `creation_rate_limit` on the project, 20 for a project created from the fixture. The window length is the process setting `RATE_LIMIT_WINDOW`, 60 seconds in the fixture.

The key is `sandbox-api:ratelimit:create:{projectUUID}`. One Lua script decides admission. The first admission in a window sets the key to `1` with a millisecond TTL. A later admission increments the key and leaves that TTL in place. A count already at the limit is denied and returns the remaining TTL. Corrupt values are deleted and the attempt is denied as an unavailable decision.

The admission client sets `MaxRetries` to `-1`. go-redis would otherwise retry a timed-out script, and a retry could count the same attempt twice. When Redis returns an error, or the script reports invalid policy or invalid state, creation fails closed with `503` and inserts nothing.

The check runs after membership is confirmed and before the project row is locked. A denied window returns `429 RATE_LIMIT_EXCEEDED` and `Retry-After`. An allowed unit stays consumed when the later quota check or the database write fails. Listing and stopping do not call this script. Members of one project share the key. Another project has another key.

A fixed window can admit a burst when one window expires and the next begins. That bound is accepted for this algorithm.

## PostgreSQL quota enforcement

Running occupancy is `CountRunningSandboxes`: the number of rows for the project whose status is `running`. The comparison uses `max_running_sandboxes` on the locked project row. A count at or above that limit returns `ErrQuotaExceeded`, which the handler maps to `429 QUOTA_EXCEEDED` without `Retry-After`.

The count and the insert run in the same locked transaction. There is no separate counter to drift from the rows. Stopped rows remain stored and drop out of the count because the status predicate excludes them. A partial index supports that count.

The quota value is the one stored on the project. It is the process default at creation time, and it is not rewritten when the process default changes.

## Project-level synchronization

`withLockedProject` is the shared protocol for a write that changes running occupancy. It begins a transaction, calls `LockAuthorizedProject`, runs the callback, and commits. The lock statement is the authorized project read plus `FOR UPDATE OF p`. The membership join is what makes the lock belong to a member. The running count is a separate statement so the lock stays a single-row project lock.

Sandbox creation calls this after Redis admission: lock, count, insert. Sandbox termination calls it after the authorized sandbox read: lock, conditional stop. The callback does not call Redis, so the database lock is not held across a network round trip to the rate limiter.

Project creation and membership insertion use their own transactions. They do not take this lock, because they do not change running occupancy. A later operation that does change occupancy uses `withLockedProject`.

Two quota writes for the same project therefore run one at a time. Writes for different projects lock different rows and proceed independently.

## Transaction isolation

Project creation, membership addition, sandbox creation, and sandbox termination begin transactions at `READ COMMITTED`. The quota critical section does not rely on a higher isolation level to see a stable count. `FOR UPDATE` on the project row orders those writers. The count that decides admission is the count of rows committed by the previous holder of that lock.

A failed callback rolls the transaction back. The handler returns success only after `Commit` returns. An uncertain commit is not reported as success and is not replayed. Rollback of project creation removes the project and the creator membership together, because they share one transaction. Rollback of sandbox creation leaves no new sandbox row. Rollback of termination leaves the previous status and `stopped_at`.

## Lifecycle transitions

`CreateSandbox` inserts `status` `running` and a null `stopped_at`. That is the only insert path.

`StopRunningSandbox` sets `stopped` and `stopped_at` to the later of the transaction clock and `created_at`, and only where the row is still `running` and still in the locked project. The first committed stop is the lifecycle transition. A second stop matches no row, the transaction still commits, and the handler returns `204`. The original `stopped_at` stays. No second slot is released.

There is no transition back to `running`, and there is no delete.

## Idempotency semantics

| Operation | Result of a repeat |
| --- | --- |
| User registration | One row per issuer and subject. A repeat login returns that row. |
| OAuth callback state | Single use. The consume script deletes it. |
| Project creation | Each commit inserts a distinct project and creator membership. |
| Project listing | Read only. |
| Membership addition | One row per project and user. `ON CONFLICT DO NOTHING` keeps the original `created_at`. The first response is `201`. A repeat is `200`. |
| Sandbox creation | Each commit inserts a distinct sandbox. |
| Sandbox listing | Read only. |
| Sandbox termination | One transition. A repeat is `204` and keeps `stopped_at`. |

A lost `201` can leave the new project or sandbox in place. The client cannot safely send the same create again and assume it will observe the original row.

## Pagination

Project and sandbox lists use the keyset `(created_at, id)` descending. The request `limit` defaults to 50 and must be from 1 to 100. The query reads one extra row. `Trim` returns at most `limit` items. `next_cursor` is set when that extra row was present, and it names the last item actually returned. An empty or final page has `next_cursor` null.

The cursor is unpadded URL-safe base64 of a JSON object: `version` 1, `collection` (`projects` or `sandboxes`), `scope`, `created_at`, and `id`. For projects, `scope` is the authenticated user id. For sandboxes, `scope` is the project id. A cursor whose version, collection, or scope does not match the request is `400`. Clients treat the token as opaque.

The page is the rows visible to that statement. It is not a snapshot held across requests. A commit that lands between two page reads can change what the following cursor returns.

## Dependency failure boundaries

| Dependency | Effect |
| --- | --- |
| PostgreSQL unavailable on a mutation or an authorized read | `503` for that request. Occupancy is whatever rows already committed. |
| Redis unavailable during login state save or consume | `503`. No callback completion. |
| Redis unavailable during creation, or an untrustworthy admission script | `503`. No insert. |
| Redis unavailable during list or stop | Those operations continue when the caller is authenticated and PostgreSQL is available. |
| Redis restart | Login state and creation windows may be empty. Business rows stay. |
| Hydra unavailable while a cached signing key can verify the token | Verification uses the cache. |
| Hydra unavailable and no usable signing key | `503`. |
| Hydra restart | In-memory sessions and the OAuth client are gone. The caller starts a new login after the client is bootstrapped again. |
| API process restart | PostgreSQL remains the record of users, projects, memberships, sandboxes, and occupancy. |

Error bodies contain a stable code and a fixed message. They omit SQL, stack traces, tokens, and upstream response bodies. `500` is reserved for an unexpected application failure.

## Design trade-offs

The running count under a project-row lock stays equal to committed `running` rows. The lock is held only for that count and the following insert or update.

The fixed window is one Redis key and one script. The TTL is set once, so the allowance is predictable for a project. The cost is a possible burst at the boundary between windows.

Disabling admission retries protects the counter from a second increment. The OAuth client keeps normal Redis retries because reading or saving state can be retried, and the consume script deletes the key only after the binding matches.

Opaque cursors keep the sort key inside the API. Encoding the user or project in `scope` stops a cursor captured for one list from being applied to another.

`404` for a non-member uses the same body as a missing id. The API has no separate disclosure of a resource the caller cannot access. The contract reserves `403` for an explicit permission denial, and these operations do not emit it.

Login requires the browser cookie because the Redis record is bound to the client that started it. That blocks a stolen `code` and `state` from being completed by a different client that never received the cookie.

## Future scalability considerations

The current column is what this repository does. The evolution column is future work. A later change still has to preserve membership checks, quota correctness, and the committed lifecycle.

| Concern | Current implementation | Potential evolution |
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

Replica reads, if introduced later, would be limited to responses that can tolerate replica lag. Quota and lifecycle commits would stay on the primary. An occupancy counter, if introduced later, would move inside the same locked transaction as the row change. A different rate algorithm, if introduced later, would still consume an admission before the quota transaction and would still fail closed when the store cannot decide.

## Documented limitations

- Hydra's in-memory sessions and OAuth client disappear on restart.
- Redis data loss resets login transactions and creation windows.
- JWT verification does not revoke a token before expiry.
- Project creation and sandbox creation are each a new row on every commit.
- An uncertain create is not replayed automatically.
- A keyset page is a position in the order, not a frozen cross-request snapshot.
- Quota-affecting operations on one project are serialized by the project row lock.
- Every writer that changes running occupancy follows that lock protocol.
- The service states no production-scale throughput.
- The service provisions no compute for a sandbox row.
