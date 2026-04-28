# devcontainer-manager

A local service/container for managing and interacting with running devcontainers.

- [Mutagen](https://github.com/mutagen-io/mutagen) integration to sync files at runtime.
- [T3 Code](https://github.com/pingdotgg/t3code) proxy for interaction with Codex or Claude Code in running devcontainer environments. See related [devcontainer features](https://github.com/boblangley/features).

## Run locally

```bash
go run ./cmd/devcontainer-manager \
  -config ./config.example.yaml \
  -listen 127.0.0.1:8787
```

Open <http://localhost:8787/> for the Tailwind/shadcn-style dashboard.

## Container image

```bash
docker build -t devcontainer-manager .
docker run --rm \
  -p 127.0.0.1:8787:8787 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD/config.example.yaml:/etc/devcontainer-manager/config.yaml:ro" \
  devcontainer-manager
```