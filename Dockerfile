FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/devcontainer-manager ./cmd/devcontainer-manager

FROM debian:bookworm-slim
ARG MUTAGEN_VERSION=0.18.1
RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates curl tar \
  && rm -rf /var/lib/apt/lists/*
RUN set -eux; \
  arch="$(dpkg --print-architecture)"; \
  case "$arch" in amd64) mutagen_arch=amd64 ;; arm64) mutagen_arch=arm64 ;; *) echo "unsupported arch: $arch" >&2; exit 1 ;; esac; \
  curl -fsSL "https://github.com/mutagen-io/mutagen/releases/download/v${MUTAGEN_VERSION}/mutagen_linux_${mutagen_arch}_v${MUTAGEN_VERSION}.tar.gz" -o /tmp/mutagen.tar.gz; \
  tar -xzf /tmp/mutagen.tar.gz -C /usr/local/bin mutagen; \
  rm /tmp/mutagen.tar.gz; \
  mutagen version
COPY --from=build /out/devcontainer-manager /usr/local/bin/devcontainer-manager
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
EXPOSE 8787
ENTRYPOINT ["/entrypoint.sh"]
