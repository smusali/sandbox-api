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
