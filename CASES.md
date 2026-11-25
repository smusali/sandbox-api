# Sandbox API Acceptance Cases

This document defines the observable behavior of the Sandbox API. The cases establish implementation correctness. They are not an automated test suite, fixture framework, or load test.

Each scenario starts from its own preconditions unless a later scenario says otherwise. Requests use Hydra-issued access tokens and application-generated UUIDs. Actor and resource names are aliases for those values.

## 1. Shared definitions

### 1.1. Actors

| Actor | Description |
| --- | --- |
| User A | Registered user and creator of Project A |
| User B | Registered user who can become a member of Project A |
| User C | Registered user without membership in Project A |
| Anonymous Client | Client without valid authentication |
| API Client | Developer application or automated agent acting for a user |
| Hydra | External OAuth authorization provider |

### 1.2. Resources

| Resource | Description |
| --- | --- |
| Project A | Project created by User A |
| Project B | Independent project created by User C |
| Sandbox A | Sandbox belonging to Project A |
| Sandbox B | Sandbox belonging to Project B |

### 1.3. Policies

| Policy | Value |
| --- | --- |
| Tenancy and authorization | Project membership. Every member has the same supported permissions. |
| Identity | Verified OAuth issuer and subject. Email is optional profile metadata and is not an authentication identifier. |
| Identifiers | Server-generated UUID strings |
| Project name | Trimmed, then 1 through 255 Unicode characters. Names are not unique. |
| Running sandbox quota | 10 per new project, persisted on the project |
| Creation rate limit | 20 admitted attempts per project per 60-second fixed window, persisted on the project |
| Rate window | Starts on the first admitted creation attempt. Later admitted attempts keep the original expiration. |
| OAuth state lifetime | 5 minutes, single use, bound to the initiating browser |
| JSON body limit | 16 KiB |
| Pagination | Keyset cursor. Default 50, minimum 1, maximum 100. Order is `created_at DESC, id DESC`. |
| Sandbox states | New records are `running`. The only transition is `running` to `stopped`. `stopped` is terminal. |
| Historical records | Stopped sandboxes stay persisted and stay visible in listings. |
| Limit changes | A change to the default limits applies to projects created afterward. Existing projects keep their persisted limits. |

PostgreSQL is authoritative for users, projects, memberships, sandbox lifecycle, and running occupancy. Running occupancy is the count of committed `running` sandboxes for that project. Redis holds temporary OAuth state and the temporary creation-rate allowance. Hydra issues access tokens. The API does not collect passwords or mint tokens.

### 1.4. Endpoints

| Method | Endpoint | Success |
| --- | --- | --- |
| GET | `/auth/login` | 302 |
| GET | `/auth/callback` | 200 |
| POST | `/v1/projects` | 201 |
| GET | `/v1/projects` | 200 |
| POST | `/v1/projects/{id}/members` | 201 or 200 |
| POST | `/v1/projects/{id}/sandboxes` | 201 |
| GET | `/v1/projects/{id}/sandboxes` | 200 |
| DELETE | `/v1/sandboxes/{id}` | 204 |

`/auth/login` and `/auth/callback` do not require a preexisting Bearer token. Every `/v1` route requires exactly one verified access JWT and a registered application user.

### 1.5. Resource representations

Project:

```json
{
  "id": "6d502949-7e8c-4f47-8b0c-3f1e3b913001",
  "name": "Agent Development",
  "created_at": "2026-11-24T20:00:00Z"
}
```

Membership:

```json
{
  "project_id": "6d502949-7e8c-4f47-8b0c-3f1e3b913001",
  "user_id": "de498973-c030-4fc2-b075-4b5dbb421002",
  "created_at": "2026-11-24T20:05:00Z"
}
```

Sandbox:

```json
{
  "id": "7719ac21-1b89-4865-89b5-0ed2f8721003",
  "project_id": "6d502949-7e8c-4f47-8b0c-3f1e3b913001",
  "status": "running",
  "created_at": "2026-11-24T20:10:00Z",
  "stopped_at": null
}
```

Collections:

```json
{
  "items": [],
  "next_cursor": null
}
```

Successful callback:

```json
{
  "access_token": "<hydra-issued-access-token>",
  "token_type": "Bearer",
  "expires_in": 3600,
  "user_id": "11111111-1111-4111-8111-111111111111"
}
```

`expires_in` is present when the token response provides it. The callback sets `Cache-Control: no-store`. Access tokens, refresh tokens, and ID tokens do not appear in redirect URLs, query parameters, or user records.

Timestamps are UTC RFC 3339. Property names are `snake_case`. Empty collections use an empty `items` array and `next_cursor: null`. A running sandbox has `stopped_at: null`. `204 No Content` has no body.

Pagination query parameters are `limit` and `cursor`. A sandbox cursor is scoped to its project. A project cursor is scoped to the authenticated user. A cursor is not authorization evidence. The same page-size and ordering rules apply to both collections.

### 1.6. Creation admission order

Sandbox creation proceeds in this order:

1. Authenticate the caller.
2. Validate the project UUID and the body.
3. Confirm project membership.
4. Admit the attempt on that project's creation-rate allowance.
5. Serialize with other quota-affecting mutations for that project, count committed running sandboxes, and insert one `running` sandbox when the count is below the persisted quota.
6. Return `201 Created` only after that transaction commits.

A failure at a step produces the status defined below and does not continue. Authentication, validation, and membership failures consume no rate allowance. An admitted attempt that then fails quota or persistence keeps the consumed rate unit. When the rate allowance and the running quota are both exhausted, the response is `429 RATE_LIMIT_EXCEEDED`.

## 2. C-01. OAuth authentication and persistent user registration

Users sign up through the Hydra authorization-code flow. The API is the OAuth client. Hydra authenticates the user and issues tokens.

**Given** Hydra is available, the OAuth client is configured, and PostgreSQL and Redis are available.

**When** a user completes the authorization-code flow.

**Then**

- `GET /auth/login` stores a cryptographically random state and an S256 PKCE verifier, binds that transaction to the initiating browser, and redirects the browser to Hydra with `302`.
- `GET /auth/callback` checks state, browser binding, expiration, and single-use semantics, consumes the transaction atomically, exchanges the code with the PKCE verifier, and verifies the access token before any user write.
- The verified issuer and subject create one application user or resolve the existing one.
- The `200` response contains `access_token`, `token_type`, `user_id`, and `expires_in` when available, with `Cache-Control: no-store`.
- The application user ID is stable. Email may be null. A later change of email keeps the same user ID and creation timestamp.
- The API never receives or stores the user's password, and it does not persist raw access tokens, refresh tokens, or client secrets on the user record.

| ID | Scenario | Result | Persistent state |
| --- | --- | --- | --- |
| C-01.01 | First successful login | `200` with the new user ID | Exactly one user for that issuer and subject |
| C-01.02 | Repeated login | `200` with the existing user ID | No additional user |
| C-01.03 | Concurrent first-time logins | Each successful response refers to the same user ID | Exactly one user |
| C-01.04 | Missing OAuth state | `400 INVALID_OAUTH_STATE` | No user write |
| C-01.05 | Incorrect OAuth state | `400 INVALID_OAUTH_STATE` | No user write |
| C-01.06 | Browser binding mismatch | `400 INVALID_OAUTH_STATE` | The other browser's transaction stays unconsumed |
| C-01.07 | Expired authorization state | `400 INVALID_OAUTH_STATE` | A new login is required |
| C-01.08 | Replayed callback | `400 INVALID_OAUTH_STATE` | Consumed state stays consumed |
| C-01.09 | Missing authorization code | `400 OAUTH_AUTHORIZATION_FAILED` | No user write |
| C-01.10 | Invalid or expired authorization code | `400 OAUTH_AUTHORIZATION_FAILED` | No unverified user |
| C-01.11 | User denies consent | `400 OAUTH_AUTHORIZATION_FAILED` | No user write |
| C-01.12 | Token exchange fails | `400 OAUTH_AUTHORIZATION_FAILED` when Hydra reports authorization failure | No unverified user |
| C-01.13 | Verified identity has no email | `200` | User is created with null email |
| C-01.14 | Existing user's email changes | `200` with the original user ID | Same user record |
| C-01.15 | Invalid upstream token response | `502 OAUTH_PROVIDER_ERROR` | No unverified user |
| C-01.16 | Redis unavailable during state handling | `503 SERVICE_UNAVAILABLE` | No successful registration |
| C-01.17 | PostgreSQL persistence fails | `500 INTERNAL_ERROR`, or `503 SERVICE_UNAVAILABLE` when PostgreSQL is unavailable | No partial user is reported |
| C-01.18 | Hydra restarts during authentication | The flow restarts when the prior transaction or session is gone | No unverified user |

An unexpected or malformed token response follows C-01.15. An authorization failure reported by Hydra follows C-01.12. Concurrent callbacks cannot consume the same state twice.

## 3. C-02. JWT API authentication

Users call the platform with OAuth access JWTs. Authentication attaches the persisted application user. It does not grant project membership.

**Given** User A has completed registration.

**When** User A calls a protected route with a valid access token.

**Then** the request is associated with User A's persisted identity. The handler receives that user ID from the verified issuer and subject. A client-supplied user ID never replaces it.

Protected routes reject every other credential with `401 UNAUTHENTICATED` and `WWW-Authenticate: Bearer`. A supplied credential that fails verification may include `error="invalid_token"` on that challenge. A valid access token whose application user does not exist is also `401 UNAUTHENTICATED`. Protected routes do not create users.

Accepted access tokens are Hydra RS256 JWTs with a verified signature, the configured issuer, a nonempty subject, a required expiration, and the configured audience and access-token purpose. `nbf` and `iat` are enforced when present. ID tokens and refresh tokens are rejected even when Hydra signed them.

An unknown signing key causes one bounded refresh of the trusted JWKS. If the key is then known, verification continues. If it remains unknown, the result is `401 UNAUTHENTICATED`. Signing-key URLs inside the token are not fetched. When a cached trusted key can verify the token, Hydra being unavailable does not by itself fail the request. When verification requires a JWKS retrieval and that retrieval is unavailable, the result is `503 SERVICE_UNAVAILABLE`.

| ID | Scenario | Result | Persistent state |
| --- | --- | --- | --- |
| C-02.01 | Missing Authorization header | `401 UNAUTHENTICATED` | Unchanged |
| C-02.02 | Empty Bearer token | `401 UNAUTHENTICATED` | Unchanged |
| C-02.03 | Unsupported authentication scheme | `401 UNAUTHENTICATED` | Unchanged |
| C-02.04 | Malformed JWT | `401 UNAUTHENTICATED` | Unchanged |
| C-02.05 | Expired JWT | `401 UNAUTHENTICATED` | Unchanged |
| C-02.06 | Missing expiration | `401 UNAUTHENTICATED` | Unchanged |
| C-02.07 | Token not yet valid | `401 UNAUTHENTICATED` | Unchanged |
| C-02.08 | Invalid signature | `401 UNAUTHENTICATED` | Unchanged |
| C-02.09 | Unexpected issuer | `401 UNAUTHENTICATED` | Unchanged |
| C-02.10 | Unexpected or missing audience | `401 UNAUTHENTICATED` | Unchanged |
| C-02.11 | Unsupported signing algorithm | `401 UNAUTHENTICATED` | Unchanged |
| C-02.12 | Unsigned JWT | `401 UNAUTHENTICATED` | Unchanged |
| C-02.13 | Missing or empty subject | `401 UNAUTHENTICATED` | Unchanged |
| C-02.14 | ID token used as an access token | `401 UNAUTHENTICATED` | Unchanged |
| C-02.15 | Refresh token used as an access token | `401 UNAUTHENTICATED` | Unchanged |
| C-02.16 | Unknown signing key | One bounded trusted JWKS refresh | Unchanged |
| C-02.17 | Key still unknown after refresh | `401 UNAUTHENTICATED` | Unchanged |
| C-02.18 | Valid token without a registered user | `401 UNAUTHENTICATED` | No user is created |
| C-02.19 | Hydra unavailable with a usable cached key | Verification continues under the cache policy | Unchanged |
| C-02.20 | Required JWKS retrieval unavailable | `503 SERVICE_UNAVAILABLE` | Unchanged |
| C-02.21 | Client supplies another user's identity | Authenticated principal stays unchanged | Unchanged |

Multiple or ambiguous credentials, including more than one Bearer credential, are `401 UNAUTHENTICATED`.

## 4. C-03. Project creation and discovery

**Given** User A is authenticated and registered.

**When** User A creates a project.

**Then** the project and the creator membership commit together. The response is `201 Created` with `id`, `name`, and `created_at`. The server assigns the project ID, the creation timestamp, and the default limits of 10 running sandboxes and 20 creation attempts per 60-second window. The creator's identity comes from the access token.

`GET /v1/projects` returns `200` and a page of projects where User A is a member, including projects User A created and projects shared with User A. Projects without that membership are absent. A user with no projects receives `items: []` and `next_cursor: null`.

Request:

```json
{
  "name": "Agent Development"
}
```

| ID | Scenario | Result | Persistent state |
| --- | --- | --- | --- |
| C-03.01 | User has no projects | `200` and an empty collection | Unchanged |
| C-03.02 | Missing project name | `400 INVALID_REQUEST` | No project |
| C-03.03 | Empty project name | `400 INVALID_REQUEST` | No project |
| C-03.04 | Whitespace-only name | `400 INVALID_REQUEST` | No project |
| C-03.05 | Surrounding whitespace | `201` after trim | Stored name has no surrounding whitespace |
| C-03.06 | Name longer than 255 Unicode characters | `400 INVALID_REQUEST` | No project |
| C-03.07 | Duplicate project name | `201` | A distinct project and creator membership |
| C-03.08 | Client supplies quota values | `400 INVALID_REQUEST` | No project |
| C-03.09 | Client supplies a creator ID | `400 INVALID_REQUEST` | No project |
| C-03.10 | Creator membership insertion fails | `500 INTERNAL_ERROR`, or `503 SERVICE_UNAVAILABLE` when PostgreSQL is unavailable | Project and membership both roll back |
| C-03.11 | Concurrent project creation | Each success is `201` | Independent projects |
| C-03.12 | User belongs to multiple projects | `200` pages through every accessible project | Unchanged |
| C-03.13 | Shared and self-created projects | Both appear | Unchanged |
| C-03.14 | Unrelated project | Excluded from the collection | Unchanged |
| C-03.15 | Invalid pagination parameters | `400 INVALID_REQUEST` | Unchanged |
| C-03.16 | Project transaction fails | `500 INTERNAL_ERROR`, or `503 SERVICE_UNAVAILABLE` when PostgreSQL is unavailable | No partial project or membership |
| C-03.17 | Repeated successful POST | Another `201` is allowed | Another distinct project |

## 5. C-04. Collaborative project membership

**Given** User A belongs to Project A and User B is registered and is not yet a member.

**When** User A adds User B.

**Then** User B becomes a member with the same supported permissions as User A. A new membership returns `201 Created`. Adding an existing member, including adding oneself, returns `200 OK` and the original membership, including its original `created_at`.

After commit, User B can list Project A, use its existing and future sandboxes, and add another registered user. Membership in Project A grants nothing on Project B.

The requester's membership is checked before the target user is looked up. A non-member receives `404 NOT_FOUND` and learns nothing about the target user or about whether the project exists.

Request:

```json
{
  "user_id": "de498973-c030-4fc2-b075-4b5dbb421002"
}
```

| ID | Scenario | Result | Persistent state |
| --- | --- | --- | --- |
| C-04.01 | Add a registered user | `201 Created` | One new membership |
| C-04.02 | Add an existing member | `200 OK` and the existing membership | Original timestamp kept |
| C-04.03 | Add oneself | `200 OK` | Original timestamp kept |
| C-04.04 | Concurrent duplicate additions | One success creates the row; the other observes it | Exactly one membership |
| C-04.05 | Target user missing | `404 NOT_FOUND` | No membership |
| C-04.06 | Target user never registered | `404 NOT_FOUND` | No membership |
| C-04.07 | Invalid target user UUID | `400 INVALID_REQUEST` | No membership |
| C-04.08 | Invalid project UUID | `400 INVALID_REQUEST` | No membership |
| C-04.09 | Project missing | `404 NOT_FOUND` | No membership |
| C-04.10 | Requester is not a member | `404 NOT_FOUND` | No membership and no target disclosure |
| C-04.11 | Membership insertion fails | `500 INTERNAL_ERROR`, or `503 SERVICE_UNAVAILABLE` when PostgreSQL is unavailable | No access is granted |
| C-04.12 | New member uses an existing sandbox | Permitted after commit | Unchanged after the membership commit |
| C-04.13 | New member adds another registered user | Permitted after commit | One further membership |
| C-04.14 | User belongs to several projects | Each membership stands alone | Independent memberships |
| C-04.15 | Repeated addition | `200 OK` | Original timestamp kept |

## 6. C-05. Multi-tenant authorization and resource isolation

**Given** User C has no membership in Project A. User A and User B belong to Project A. User C belongs to Project B. Sandbox A belongs to Project A. Sandbox B belongs to Project B.

**When** User C requests Project A or Sandbox A.

**Then** the API returns `404 NOT_FOUND` with the same public body used when the resource does not exist. Authentication has already succeeded. No project, membership, sandbox, quota, or rate-limit state changes.

| Operation | User A | User B | User C on Project A |
| --- | --- | --- | --- |
| Discover Project A | Allowed | Allowed | Excluded from listings |
| Add a member to Project A | Allowed | Allowed | `404 NOT_FOUND` |
| Create a sandbox in Project A | Allowed | Allowed | `404 NOT_FOUND` |
| List Project A sandboxes | Allowed | Allowed | `404 NOT_FOUND` |
| Stop Sandbox A | Allowed | Allowed | `404 NOT_FOUND` |
| Access Project B sandboxes | `404 NOT_FOUND` | `404 NOT_FOUND` | Allowed |

Termination receives a sandbox ID. The API loads that sandbox's persisted project and checks membership in that project. A client-supplied project association is ignored for an existing sandbox. Allowed operations still pass validation and the applicable limits.

| ID | Scenario | Result | Persistent state |
| --- | --- | --- | --- |
| C-05.01 | Non-member requests a project UUID | `404 NOT_FOUND` | Unchanged |
| C-05.02 | Non-member requests a sandbox UUID | `404 NOT_FOUND` | Unchanged |
| C-05.03 | Non-member lists sandboxes | `404 NOT_FOUND` | Unchanged |
| C-05.04 | Non-member creates a sandbox | `404 NOT_FOUND` | No sandbox and no rate use |
| C-05.05 | Non-member stops a sandbox | `404 NOT_FOUND` | No lifecycle or quota change |
| C-05.06 | Non-member changes membership | `404 NOT_FOUND` | No membership change |
| C-05.07 | Resource does not exist | `404 NOT_FOUND` | Unchanged |
| C-05.08 | Resource exists and is inaccessible | Same public `404 NOT_FOUND` body as C-05.07 | Unchanged |
| C-05.09 | User belongs to several projects | Each project is authorized on its own | Unchanged |
| C-05.10 | Client supplies another user's UUID | No impersonation | Caller identity unchanged |
| C-05.11 | Unauthorized sandbox creation | `404 NOT_FOUND` | Rate allowance unchanged |
| C-05.12 | Unauthorized termination | `404 NOT_FOUND` | Lifecycle and occupancy unchanged |
| C-05.13 | Cursor belongs to another project | `400 INVALID_REQUEST` | Unchanged |
| C-05.14 | Anonymous protected request | `401 UNAUTHENTICATED` | Unchanged |

Membership in one project grants nothing on another. Knowing a UUID grants nothing. A cursor cannot bypass membership, and every page is authorized on its own.

## 7. C-06. Sandbox creation under available capacity

**Given** User A belongs to Project A, the project has fewer than 10 running sandboxes, and the current window has admitted fewer than 20 creation attempts.

**When** User A submits a valid creation request.

**Then** one sandbox is committed in the `running` state and the response is `201 Created`. The server assigns `id`, `project_id`, `status`, `created_at`, and `stopped_at: null`. An empty body and an empty JSON object are both valid. No client configuration is required.

```json
{
  "id": "7719ac21-1b89-4865-89b5-0ed2f8721003",
  "project_id": "6d502949-7e8c-4f47-8b0c-3f1e3b913001",
  "status": "running",
  "created_at": "2026-11-24T20:10:00Z",
  "stopped_at": null
}
```

A successful create increases running occupancy by one, leaves stopped records unchanged, and consumes one rate unit. Example: occupancy 3 running and 2 stopped, with 5 rate units already consumed, becomes 4 running, 2 stopped, and 6 consumed.

| ID | Scenario | Result | Persistent state |
| --- | --- | --- | --- |
| C-06.01 | Empty body, capacity available | `201 Created` | One new `running` sandbox and one rate unit consumed |
| C-06.02 | Empty JSON object, capacity available | `201 Created` | One new `running` sandbox and one rate unit consumed |
| C-06.03 | Malformed JSON | `400 INVALID_REQUEST` | No sandbox and no rate use |
| C-06.04 | Client supplies status | `400 INVALID_REQUEST` | No sandbox and no rate use |
| C-06.05 | Client supplies an ID | `400 INVALID_REQUEST` | No sandbox and no rate use |
| C-06.06 | Client supplies ownership | `400 INVALID_REQUEST` | No sandbox and no rate use |
| C-06.07 | Invalid project UUID | `400 INVALID_REQUEST` | No sandbox and no rate use |
| C-06.08 | Project missing | `404 NOT_FOUND` | No sandbox and no rate use |
| C-06.09 | Requester is not a member | `404 NOT_FOUND` | No sandbox and no rate use |
| C-06.10 | Creation rate exhausted | `429 RATE_LIMIT_EXCEEDED` | No sandbox; saturated counter stays unchanged |
| C-06.11 | Running quota exhausted | `429 QUOTA_EXCEEDED` | No sandbox; the admitted rate unit stays consumed |
| C-06.12 | Both limits exhausted | `429 RATE_LIMIT_EXCEEDED` | No sandbox and no further rate increment |
| C-06.13 | Redis unavailable | `503 SERVICE_UNAVAILABLE` | No sandbox |
| C-06.14 | Database insertion fails | `500 INTERNAL_ERROR`, or `503 SERVICE_UNAVAILABLE` when PostgreSQL is unavailable | No committed sandbox |
| C-06.15 | Transaction rolls back | `500 INTERNAL_ERROR`, or `503 SERVICE_UNAVAILABLE` when PostgreSQL is unavailable | Running occupancy unchanged; admitted rate unit stays consumed |
| C-06.16 | Client disconnects before commit | In-flight work is canceled where possible | No partial sandbox |
| C-06.17 | Response lost after commit | The client may not see `201` | The sandbox may exist; a repeated POST may create another |
| C-06.18 | Commit outcome uncertain | Success is not claimed and creation is not blindly replayed | Only a confirmed commit is treated as success |

Every sandbox belongs to exactly one project. Creation of a sandbox is not idempotent.

## 8. C-07. Sandbox listing and historical records

**Given** User A belongs to Project A.

**When** User A requests `GET /v1/projects/{id}/sandboxes`.

**Then** the API returns `200` and one bounded page of Project A's running and stopped sandboxes, ordered by `created_at DESC, id DESC`. Other projects' sandboxes are absent. The page contains at most `limit` records. The final page has `next_cursor: null`.

```json
{
  "items": [
    {
      "id": "7719ac21-1b89-4865-89b5-0ed2f8721003",
      "project_id": "6d502949-7e8c-4f47-8b0c-3f1e3b913001",
      "status": "running",
      "created_at": "2026-11-24T20:10:00Z",
      "stopped_at": null
    },
    {
      "id": "7719ac21-1b89-4865-89b5-0ed2f8721004",
      "project_id": "6d502949-7e8c-4f47-8b0c-3f1e3b913001",
      "status": "stopped",
      "created_at": "2026-11-24T20:00:00Z",
      "stopped_at": "2026-11-24T20:05:00Z"
    }
  ],
  "next_cursor": null
}
```

| ID | Scenario | Result | Persistent state |
| --- | --- | --- | --- |
| C-07.01 | Project contains no sandboxes | `200` with `items: []` and `next_cursor: null` | Unchanged |
| C-07.02 | Running records exist | Included | Unchanged |
| C-07.03 | Stopped records exist | Included | Unchanged |
| C-07.04 | Other projects contain records | Those records are excluded | Unchanged |
| C-07.05 | Requester is not a member | `404 NOT_FOUND` | Unchanged |
| C-07.06 | Project does not exist | `404 NOT_FOUND` | Unchanged |
| C-07.07 | Limit omitted | Page size 50 | Unchanged |
| C-07.08 | Limit is 1 | At most one record | Unchanged |
| C-07.09 | Limit is 100 | At most 100 records | Unchanged |
| C-07.10 | Limit is zero or negative | `400 INVALID_REQUEST` | Unchanged |
| C-07.11 | Limit is greater than 100 | `400 INVALID_REQUEST` | Unchanged |
| C-07.12 | Limit is not an integer | `400 INVALID_REQUEST` | Unchanged |
| C-07.13 | Malformed cursor | `400 INVALID_REQUEST` | Unchanged |
| C-07.14 | Cursor belongs to another project | `400 INVALID_REQUEST` | Unchanged |
| C-07.15 | Equal creation timestamps | UUID ordering breaks the tie | Unchanged |
| C-07.16 | Final page | `next_cursor: null` | Unchanged |
| C-07.17 | Redis unavailable | Listing still succeeds when authentication and PostgreSQL are available | Unchanged |
| C-07.18 | Large historical collection | Only the requested page is returned | Unchanged |
| C-07.19 | New records arrive during pagination | Keyset order is preserved; pages are not a frozen snapshot | Unchanged by listing |
| C-07.20 | A sandbox stops during pagination | The response shows the committed state visible to that page | Listing itself changes nothing |

Listing consumes no rate allowance and does not change occupancy. The same paging rules apply to `GET /v1/projects`, with the cursor scoped to the caller.

## 9. C-08. Sandbox termination and idempotent lifecycle

**Given** User A is a member of the project that owns Sandbox A, and Sandbox A is `running`.

**When** User A calls `DELETE /v1/sandboxes/{id}`.

**Then** the sandbox moves from `running` to `stopped`, `stopped_at` is set, the row remains, and the response is `204 No Content` with an empty body. Running occupancy decreases by one. The creation-rate counter is unchanged. A later delete of the same sandbox returns `204` and keeps the original `stopped_at`. There is no restart operation.

Stopping a sandbox at quota 10 running and 5 stopped, with some rate units already consumed, leaves 9 running, 6 stopped, the same total row count, and the same rate consumption.

| ID | Scenario | Result | Persistent state |
| --- | --- | --- | --- |
| C-08.01 | Stop a running sandbox | `204 No Content` | `stopped`, `stopped_at` set, one slot released |
| C-08.02 | Stop an already stopped sandbox | `204 No Content` | Unchanged |
| C-08.03 | Repeated termination | `204 No Content` | Original `stopped_at` kept |
| C-08.04 | Concurrent termination of the same sandbox | One transition; later calls return `204` | One `stopped_at` |
| C-08.05 | Sandbox does not exist | `404 NOT_FOUND` | Unchanged |
| C-08.06 | Sandbox belongs to an unauthorized project | `404 NOT_FOUND` | Unchanged |
| C-08.07 | Invalid sandbox UUID | `400 INVALID_REQUEST` | Unchanged |
| C-08.08 | Active quota exhausted | `204` for an authorized stop | One slot released |
| C-08.09 | Creation rate exhausted | `204` for an authorized stop | Rate counter unchanged |
| C-08.10 | Redis unavailable | `204` when authentication and PostgreSQL are available | Same as a normal stop |
| C-08.11 | Database update fails | `500 INTERNAL_ERROR`, or `503 SERVICE_UNAVAILABLE` when PostgreSQL is unavailable | Previously committed state remains |
| C-08.12 | Termination rolls back | `500 INTERNAL_ERROR`, or `503 SERVICE_UNAVAILABLE` when PostgreSQL is unavailable | No capacity released |
| C-08.13 | Successful response lost | A repeat returns `204` | Original `stopped_at` kept |
| C-08.14 | Attempt to restart a stopped sandbox | No supported operation | Remains `stopped` |
| C-08.15 | Several members stop the same sandbox | One transition | Later calls are idempotent success |
| C-08.16 | Creation races with termination | The committed serialized outcome stands | Occupancy matches committed running rows |

Termination does not delete the row, does not change ownership, and is reported only after commit. Only a `running` sandbox can enter `stopped`.

## 10. C-09. Distributed project-level rate limiting

**Given** Project A allows 20 admitted creation attempts per 60-second window, and User A and User B are members.

**When** members submit sandbox creation requests.

**Then** Redis atomically enforces one shared allowance for that project. Admission lets the request continue to quota enforcement. Admission is not sandbox creation. API instances share the same allowance. Another project has its own bucket.

| Request in a fresh window | Counter before | Counter after | Decision |
| --- | --- | --- | --- |
| First | 0 | 1 | Admit and set the window expiration |
| Second through nineteenth | 1 through 18 | 2 through 19 | Admit, expiration unchanged |
| Twentieth | 19 | 20 | Admit the final unit |
| Twenty-first | 20 | 20 | `429 RATE_LIMIT_EXCEEDED` |
| First after the window expires | 0 | 1 | New window |

The rejection is HTTP `429` with `Retry-After` set to the whole seconds remaining in the window.

```json
{
  "error": {
    "code": "RATE_LIMIT_EXCEEDED",
    "message": "The project has reached its sandbox creation request limit."
  }
}
```

A fixed window may admit a burst at the end of one window and another burst at the start of the next. That boundary behavior is accepted.

| ID | Scenario | Result | Persistent state |
| --- | --- | --- | --- |
| C-09.01 | First attempt in a new window | Admitted | Counter starts at 1 with an expiration |
| C-09.02 | Attempts while capacity remains | Admitted and incremented | Expiration unchanged |
| C-09.03 | Twentieth attempt | Admitted | Counter reaches 20 |
| C-09.04 | Twenty-first attempt | `429 RATE_LIMIT_EXCEEDED` | Counter stays 20 |
| C-09.05 | Different members create sandboxes | They share one bucket | One project counter |
| C-09.06 | Same user, different projects | Each project is independent | Separate counters |
| C-09.07 | Concurrent requests compete for the final unit | At most one is admitted | Counter does not pass 20 |
| C-09.08 | Requests reach different API instances | They share one allowance | One project counter |
| C-09.09 | Window expires | The next admitted request opens a new window | Fresh counter and expiration |
| C-09.10 | Further admitted request in the open window | Admitted | Original expiration kept |
| C-09.11 | Unauthorized creation | `404 NOT_FOUND` | No rate use |
| C-09.12 | Anonymous creation | `401 UNAUTHENTICATED` | No rate use |
| C-09.13 | Syntactically invalid creation | `400 INVALID_REQUEST` | No rate use |
| C-09.14 | Admitted request fails quota | `429 QUOTA_EXCEEDED` | Rate unit remains consumed |
| C-09.15 | Admitted request fails persistence | `500 INTERNAL_ERROR`, or `503 SERVICE_UNAVAILABLE` when PostgreSQL is unavailable | Rate unit remains consumed |
| C-09.16 | Request hits a saturated bucket | `429 RATE_LIMIT_EXCEEDED` | Counter is not incremented |
| C-09.17 | Redis unavailable | `503 SERVICE_UNAVAILABLE` | No sandbox; rate enforcement stays closed |
| C-09.18 | Redis loses limiter state | Later attempts may see a fresh allowance | Durable quota is unchanged |
| C-09.19 | Counter is malformed or has no expiration | The bucket is repaired so it expires, or the request receives `503 SERVICE_UNAVAILABLE` | A permanent bucket is not left behind |
| C-09.20 | Requests span adjacent windows | Fixed-window bursts are accepted | Each window enforces its own 20 |
| C-09.21 | Listing during exhaustion | Listing is permitted | Rate counter unchanged |
| C-09.22 | Termination during exhaustion | Termination is permitted | Rate counter unchanged |

Example of rate and quota together: 10 sandboxes are running and 5 rate units are consumed. The next creation is admitted and the counter becomes 6. Quota then rejects it with `429 QUOTA_EXCEEDED`. The sixth rate unit stays consumed.

## 11. C-10. Concurrent running sandbox quota

**Given** Project A permits at most 10 concurrently running sandboxes.

**When** authorized requests create or stop sandboxes.

**Then** committed running occupancy never exceeds the persisted quota through supported operations. Members and API instances share that quota. Distinct projects are independent. Stopped rows do not count. Redis restarts do not change occupancy. After an API restart, occupancy is derived again from committed sandbox rows.

**Given** 9 sandboxes are running and two members both pass rate admission and create at the same time.

**When** their quota transactions are serialized.

**Then** one request commits a tenth `running` sandbox and returns `201`. The other observes 10 running sandboxes, inserts nothing, and returns `429 QUOTA_EXCEEDED`. Either caller may win.

```json
{
  "error": {
    "code": "QUOTA_EXCEEDED",
    "message": "The project has reached its running sandbox limit."
  }
}
```

Quota exhaustion has no `Retry-After` header. Capacity returns when a running sandbox stops, not when a clock expires.

| Operation | Running | Remaining capacity |
| --- | --- | --- |
| At the quota | 10 | 0 |
| Creation attempt | 10 | 0, `429 QUOTA_EXCEEDED` |
| Stop one running sandbox | 9 | 1 |
| Repeat that stop | 9 | 1 |
| Create again, rate permitting | 10 | 0 |

| ID | Scenario | Result | Persistent state |
| --- | --- | --- | --- |
| C-10.01 | No running sandboxes | Creation permitted when rate allows | Occupancy becomes 1 |
| C-10.02 | Occupancy below the limit | Creation permitted when rate allows | Occupancy increases by 1 |
| C-10.03 | Creation consumes the final slot | `201 Created` | Occupancy equals the quota |
| C-10.04 | Occupancy equals the quota | `429 QUOTA_EXCEEDED` | Occupancy unchanged; rate unit kept if admitted |
| C-10.05 | Occupancy already exceeds the quota | Further creation is rejected | No additional running sandbox |
| C-10.06 | Two requests compete for the final slot | At most one `201` | Occupancy does not pass the quota |
| C-10.07 | Many concurrent creations | Successes cannot exceed the free slots | Occupancy stays at or below the quota |
| C-10.08 | Different members create concurrently | They share one quota | One project occupancy |
| C-10.09 | Different API instances create concurrently | One authoritative occupancy | Committed rows remain the source |
| C-10.10 | Different projects create concurrently | Each project uses its own quota | Independent occupancy |
| C-10.11 | A running sandbox stops | `204` | One slot released |
| C-10.12 | A stopped sandbox stops again | `204` | No extra slot released |
| C-10.13 | Creation rolls back | `500 INTERNAL_ERROR`, or `503 SERVICE_UNAVAILABLE` when PostgreSQL is unavailable | Occupancy unchanged |
| C-10.14 | Termination rolls back | `500 INTERNAL_ERROR`, or `503 SERVICE_UNAVAILABLE` when PostgreSQL is unavailable | Occupancy unchanged |
| C-10.15 | Historical stopped rows accumulate | They are excluded from occupancy | Stopped rows remain stored |
| C-10.16 | Creation races with termination | Serialized committed mutations decide the result | Occupancy matches committed running rows |
| C-10.17 | Concurrent stops of distinct sandboxes | Each effective stop releases one slot | One transition per sandbox |
| C-10.18 | API process restarts | Later quota checks use persisted rows | Business rows remain |
| C-10.19 | Redis restarts | Quota checks stay accurate | Running rows unchanged |
| C-10.20 | PostgreSQL unavailable | The affected operation is rejected with `503 SERVICE_UNAVAILABLE` | No invented occupancy |
| C-10.21 | Rate admission succeeds and quota fails | `429 QUOTA_EXCEEDED` | Rate unit remains consumed |
| C-10.22 | Commit outcome uncertain | Success is not claimed and the mutation is not blindly replayed | Only a confirmed commit changes occupancy |

Creation and termination that affect the same project's occupancy are serialized. A quota count used for admission observes sandboxes already committed by the previous holder of that serialization. Every quota-affecting writer follows that same coordination. Rollback leaves the previously committed occupancy in place.

## 12. Cross-cutting acceptance cases

### 12.1. Request validation

| Scenario | Result | Persistent state |
| --- | --- | --- |
| Body larger than 16 KiB | `413 REQUEST_TOO_LARGE` | Unchanged, and creation consumes no rate unit |
| Nonempty body with an unsupported content type | `415 UNSUPPORTED_MEDIA_TYPE` | Unchanged, and creation consumes no rate unit |
| Unsupported HTTP method | `405 METHOD_NOT_ALLOWED` | Unchanged |
| Unknown route | `404 NOT_FOUND` | Unchanged |
| Unknown JSON field | `400 INVALID_REQUEST` | Unchanged |
| Trailing extra JSON value | `400 INVALID_REQUEST` | Unchanged |
| Incompatible JSON type, or a JSON null in place of an object | `400 INVALID_REQUEST` | Unchanged |
| Unexpected body on a route that does not accept one | `400 INVALID_REQUEST` | Unchanged |
| Malformed resource UUID | `400 INVALID_REQUEST` | Unchanged |
| Missing required field | `400 INVALID_REQUEST` | Unchanged |
| Client supplies a server-managed field | `400 INVALID_REQUEST` | Unchanged |

Sandbox creation is the only route that accepts an empty body. A JSON body on any route that accepts one uses a JSON content type. Invalid creation input consumes no rate allowance.

### 12.2. Error contract

Business errors use this object:

```json
{
  "error": {
    "code": "INVALID_REQUEST",
    "message": "The supplied request is invalid."
  }
}
```

| HTTP status | Code | When it is used |
| --- | --- | --- |
| 400 | `INVALID_REQUEST` | Invalid parameters, payload, or pagination |
| 400 | `INVALID_OAUTH_STATE` | Missing, incorrect, expired, mismatched, or consumed OAuth state |
| 400 | `OAUTH_AUTHORIZATION_FAILED` | Consent denial, missing or rejected code, or other authorization-code failure |
| 401 | `UNAUTHENTICATED` | Missing, ambiguous, or invalid authentication |
| 404 | `NOT_FOUND` | Missing route, missing resource, or inaccessible project resource |
| 405 | `METHOD_NOT_ALLOWED` | Method the route does not support |
| 413 | `REQUEST_TOO_LARGE` | Body above 16 KiB |
| 415 | `UNSUPPORTED_MEDIA_TYPE` | Unsupported content type for a nonempty JSON body |
| 429 | `RATE_LIMIT_EXCEEDED` | Creation window is full. Includes `Retry-After`. |
| 429 | `QUOTA_EXCEEDED` | Running occupancy is at or above the persisted quota. No `Retry-After`. |
| 500 | `INTERNAL_ERROR` | Unexpected application failure |
| 502 | `OAUTH_PROVIDER_ERROR` | Invalid or unexpected upstream OAuth response |
| 503 | `SERVICE_UNAVAILABLE` | A required dependency is unavailable |

Illustrative messages for the other common codes:

| Code | Message |
| --- | --- |
| `UNAUTHENTICATED` | A valid access token is required. |
| `NOT_FOUND` | The requested resource was not found. |
| `RATE_LIMIT_EXCEEDED` | The project has reached its sandbox creation request limit. |
| `QUOTA_EXCEEDED` | The project has reached its running sandbox limit. |

Project and sandbox authorization uses `404 NOT_FOUND` for both absence and inaccessibility. `403 FORBIDDEN` is reserved for an explicitly disclosed permission denial, and these project and sandbox operations do not use it. Unexpected database failures stay `500 INTERNAL_ERROR` or `503 SERVICE_UNAVAILABLE`.

Error bodies hide SQL text, stack traces, OAuth secrets, and internal dependency details. Mutation success is returned only after the corresponding commit.

### 12.3. Idempotency

| Operation | Behavior |
| --- | --- |
| Application user registration | One user per issuer and subject |
| OAuth callback transaction | Single use |
| Project creation | Each committed POST creates a distinct project |
| Project listing | Read only |
| Membership addition | One membership per project and user; repeats return the original row |
| Sandbox creation | Each committed POST creates a distinct sandbox |
| Sandbox listing | Read only |
| Sandbox termination | One transition; repeats return `204` and keep `stopped_at` |

A lost success response can leave a created project or sandbox in place. An uncertain commit is not reported as success and is not automatically replayed.

### 12.4. Rate limit and quota together

The values below are the project state immediately before the creation request. The caller is already authorized and the body is valid.

| Consumed creation attempts | Running sandboxes | Result |
| --- | --- | --- |
| 0 | 0 | `201 Created` |
| 19 | 9 | `201 Created` |
| 20 | 4 | `429 RATE_LIMIT_EXCEEDED` |
| 5 | 10 | `429 QUOTA_EXCEEDED`, and the rate counter becomes 6 |
| 20 | 10 | `429 RATE_LIMIT_EXCEEDED` |
| 0 after the window expires | 10 | `429 QUOTA_EXCEEDED` |
| 20 | 9 after a stop | `429 RATE_LIMIT_EXCEEDED` |
| 0 after the window expires | 9 after a stop | `201 Created` |

Stopping a sandbox does not reset the creation window. Window expiration does not release running occupancy. Listing and termination ignore the creation window.

### 12.5. Dependencies and atomicity

| Scenario | Result |
| --- | --- |
| PostgreSQL unavailable for a mutation or authorized read | `503 SERVICE_UNAVAILABLE` for that operation |
| Redis unavailable during OAuth state handling | `503 SERVICE_UNAVAILABLE` |
| Redis unavailable during creation | `503 SERVICE_UNAVAILABLE` |
| Redis unavailable during listing or termination | The operation continues when authentication and PostgreSQL are available |
| Redis restarts | OAuth state and rate windows may reset. Users, projects, memberships, sandboxes, and occupancy remain. |
| Hydra unavailable with a usable cached signing key | Verification continues |
| Hydra unavailable and the signing key cannot be retrieved | `503 SERVICE_UNAVAILABLE` |
| Hydra restarts | The caller starts a new authentication flow when the previous transaction or session is gone |
| Signing key changes | One bounded trusted JWKS refresh, then C-02.16 and C-02.17 |
| API process restarts | Persisted business state remains authoritative |
| Project transaction fails | Project and creator membership roll back together |
| Membership persistence fails | No access is granted |
| Sandbox creation transaction fails | No committed sandbox |
| Sandbox termination transaction fails | The previous lifecycle state remains |
| Client disconnects during a mutation | The transaction stays atomic |
| Stored lifecycle row is inconsistent | The inconsistent record is rejected |

### 12.6. Observable integrity

- A user is unique on `(oauth_issuer, oauth_subject)`. Both values are nonempty. Email is optional.
- A project has a nonempty trimmed name, a positive running quota, a positive creation-rate limit, and a creation timestamp. Its creator membership exists only together with the project.
- A membership references an existing project and an existing user, and the pair is unique.
- A sandbox references one project. Status is `running` or `stopped`. `stopped_at` is null while running, and a non-null timestamp at or after `created_at` while stopped.
- Default configuration changes do not rewrite limits already stored on a project.

## 13. Traceability

| Case | Rows | Behavior | Implementation |
| --- | --- | --- | --- |
| C-01 | C-01.01 through C-01.18 | OAuth registration, FR-01 | E2B-03, E2B-05, E2B-06 |
| C-02 | C-02.01 through C-02.21 | JWT authentication, FR-02 | E2B-05, E2B-07 |
| C-03 | C-03.01 through C-03.17 | Project creation and discovery, FR-03, FR-04 | E2B-03, E2B-08, E2B-09 |
| C-04 | C-04.01 through C-04.15 | Collaborative membership, FR-05 | E2B-09, E2B-10 |
| C-05 | C-05.01 through C-05.14 | Tenant isolation, FR-06 | E2B-07, E2B-08, E2B-10, E2B-12, E2B-13, E2B-14 |
| C-06 | C-06.01 through C-06.18 | Sandbox creation, FR-07 | E2B-11, E2B-12 |
| C-07 | C-07.01 through C-07.20 | Sandbox listing, FR-08 | E2B-03, E2B-08, E2B-13 |
| C-08 | C-08.01 through C-08.16 | Sandbox termination, FR-09 | E2B-12, E2B-14 |
| C-09 | C-09.01 through C-09.22 | Distributed creation rate limiting, FR-10 | E2B-11, E2B-12 |
| C-10 | C-10.01 through C-10.22 | Running sandbox quota, FR-11 | E2B-03, E2B-12, E2B-14 |

Section 12 covers shared validation, error mapping, idempotency, combined limits, dependency failures, and integrity constraints used by those cases.

| Acceptance | Covered by |
| --- | --- |
| AC-03 through AC-05, AC-09 | C-01 |
| AC-06 through AC-08 | C-02 |
| AC-10 through AC-14 | C-03 |
| AC-15 through AC-17 | C-04 |
| AC-18, AC-25 | C-05 |
| AC-19 | C-06 |
| AC-20, AC-43 | C-07 and C-03 |
| AC-21 through AC-24, AC-37 | C-08 and C-09 |
| AC-26 through AC-31 | C-09 |
| AC-32 through AC-36, AC-38 | C-10 and section 12 |
| AC-41, AC-42, AC-44 through AC-46 | Section 12 |

Environment bootstrap, generated database access, schema migrations, configuration validation, reviewer setup instructions, and the architecture write-up are outside this behavioral specification.

The following product behavior is also outside these cases: password authentication, token refresh, logout, account recovery, provider linking, project deletion or rename, invitations, dynamic plans, creation idempotency keys, role hierarchies, real sandbox compute, restart, pause, snapshots, permanent deletion, and any rate-limit algorithm other than the fixed project window.
