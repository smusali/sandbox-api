# sandbox-api

Go 1.27.4 API for authenticated projects and sandboxes.

## Local dependencies

Docker Compose runs PostgreSQL 16, Redis 7, and Ory Hydra v2.2.0.

```sh
docker compose up -d --build
```

PostgreSQL uses host port 5432, Redis uses 6379, and Hydra uses 4444 and 4445. Port 5432 must be free before the PostgreSQL container can start.

Load the fixture environment and start the API on the host:

```sh
set -a
source .env.example
set +a
go run ./cmd/api
```

The process listens on `:8080` after PostgreSQL and Redis both accept a connection.

## OAuth fixture

Hydra's issuer is `http://localhost:4444`. Signing keys are published at `http://localhost:4444/.well-known/jwks.json`.

Authorization code with S256 PKCE returns an access token and an ID token. Both are RS256 JWTs with header `typ` `JWT`. They are signed by different keys.

An access token has `aud` `urn:e2b:sandbox-api`. An ID token has `aud` `e2b-assignment`. That audience is how the API tells an access token from an ID token.

Development accounts use password `foobar`. The OAuth subject is the email address.

| Email | Subject |
| --- | --- |
| `foo@bar.com` | `foo@bar.com` |
| `bar@foo.com` | `bar@foo.com` |
| `baz@foo.com` | `baz@foo.com` |

Hydra keeps the OAuth client in memory. PostgreSQL data stays in the `postgres-data` volume. `docker compose up hydra-bootstrap` creates the client, or replaces it when the client already exists.

## Creation-rate limits

Sandbox creation uses one fixed window per project. The default allowance is 20 admitted attempts. The default window is 60 seconds. The window starts on the first admitted attempt, and later attempts do not move its expiration. Members of a project share that allowance. Distinct projects have separate allowances.

A fixed window can admit a burst at the boundary between two windows. Redis data loss can reset this temporary allowance. Running occupancy stays in PostgreSQL. An admitted attempt stays consumed when quota or persistence fails. Creation fails closed when Redis cannot make a trustworthy admission decision.

Each successful `POST /v1/projects/{id}/sandboxes` may create a distinct sandbox. The request is not idempotent, and an uncertain result is not retried. Running quota counts only sandboxes whose status is `running`. Stopped sandboxes do not consume it. Rate-limit exhaustion is decided before quota is evaluated.
