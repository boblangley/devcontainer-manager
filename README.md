# devcontainer-manager

A local service/container for managing and interacting with running devcontainers.

- [Mutagen](https://github.com/mutagen-io/mutagen) integration to sync files at runtime.
- [T3 Code](https://github.com/pingdotgg/t3code) proxy for interaction with Codex or Claude Code in running devcontainer environments. See related [devcontainer features](https://github.com/boblangley/features).

## Docker network attachment

Set `discovery.connectToNetwork` to an existing Docker network name or ID to
have DCM attach each running detected devcontainer to that network during
reconcile:

```yaml
discovery:
  connectToNetwork: "devcontainer-manager"
```

When configured, DCM also prefers the container IP from that network for
container access.

## Pairing token persistence

DCM issues browser bearer tokens during the t3 pairing bootstrap at
`POST /env/<id>/api/auth/bootstrap/bearer`. By default those tokens are kept in
memory. Set `pairingTokens.postgresUrl` to persist them in Postgres:

```yaml
pairingTokens:
  postgresUrl: "postgres://user:pass@localhost:5432/devcontainer_manager?sslmode=disable"
```

DCM creates this table if needed:

```sql
CREATE TABLE IF NOT EXISTS pairing_tokens (
  devcontainer_id  TEXT PRIMARY KEY,
  token            TEXT NOT NULL,
  expires_at       TIMESTAMPTZ NOT NULL,
  issued_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

The in-memory cache is still used for hot requests, but authorization falls
back to Postgres after a restart. Upstream t3 401s still use the existing
backend-token refresh path (`t3 auth session issue`) and update the live
container state before retrying the proxied request.

## Spawn stage API

`POST /env/<id>/spawn-stage` is an authenticated single-shot endpoint for an
upstream project-management service. It accepts the same DCM browser bearer
token used by the existing `/env/<id>/...` proxy routes.

```json
{
  "agent": "codex",
  "renderedPrompt": "Fix ENG-103 using the auth-service conventions...",
  "workspacePath": "/workspaces/auth-service",
  "scope": {
    "bankId": "comp:auth-service",
    "tags": ["card:ENG-103", "type:bug", "column:in-dev"]
  }
}
```

DCM writes `scope` inside the target container as the configured container user
(`defaults.containerUser`, or the matched rule/user metadata):

```text
<workspacePath>/.hindsight/active-retain-scope.json
```

The file contains only:

```json
{
  "bankId": "comp:auth-service",
  "tags": ["card:ENG-103", "type:bug", "column:in-dev"]
}
```

DCM then creates a t3 project/thread if needed and starts the first turn with
`renderedPrompt` as the user message. The response is:

```json
{ "threadId": "dcm-thread-..." }
```

The retain-scope file is used because t3 owns the agent process environment.
Per-session Hindsight bank/tag data cannot be injected reliably through env
vars without changing t3; hooks read this workspace-local file relative to the
agent cwd and fall back to install-time defaults if it is missing or unreadable.

## Run locally

```bash
go run ./cmd/devcontainer-manager \
  -config ./config.example.yaml \
  -listen 127.0.0.1:8787
```

Open <http://localhost:8787/> for the Tailwind/shadcn-style dashboard.

## Tests

Unit tests do not require Docker:

```bash
go test ./...
```

Docker-backed integration tests are behind the `integration` build tag:

```bash
go test -tags=integration ./internal/dockerx -run TestExecWithStdinWritesRetainScopeAsContainerUser -v
```

## Container image

```bash
docker build -t devcontainer-manager .
docker run --rm \
  -p 127.0.0.1:8787:8787 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD/config.example.yaml:/etc/devcontainer-manager/config.yaml:ro" \
  devcontainer-manager
```
