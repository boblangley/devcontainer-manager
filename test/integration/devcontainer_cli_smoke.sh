#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
FIXTURE="$(cd "$ROOT/test/fixtures/basic" && pwd)"
LISTEN="${DCM_TEST_LISTEN:-127.0.0.1:18787}"
BASE_URL="http://${LISTEN}"
CONFIG_FILE="$(mktemp)"
LOG_FILE="$(mktemp)"
MANAGER_PID=""

remove_fixture_containers() {
  docker ps -aq --filter "label=devcontainer.local_folder=${FIXTURE}" | xargs -r docker rm -f >/dev/null
}

cleanup() {
  if [[ -n "${MANAGER_PID}" ]] && kill -0 "${MANAGER_PID}" 2>/dev/null; then
    kill "${MANAGER_PID}" 2>/dev/null || true
    wait "${MANAGER_PID}" 2>/dev/null || true
  fi
  remove_fixture_containers
  rm -f "${CONFIG_FILE}" "${LOG_FILE}"
}
trap cleanup EXIT

cd "${ROOT}"

cat >"${CONFIG_FILE}" <<YAML
server:
  listen: "${LISTEN}"

t3:
  enabled: false

sync:
  enabled: false

rules:
  - name: default
    match: {}
YAML

remove_fixture_containers
devcontainer up --workspace-folder "${FIXTURE}"

go run ./cmd/devcontainer-manager \
  -config "${CONFIG_FILE}" \
  -listen "${LISTEN}" \
  >"${LOG_FILE}" 2>&1 &
MANAGER_PID="$!"

for _ in $(seq 1 60); do
  if curl -fsS "${BASE_URL}/healthz" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "${MANAGER_PID}" 2>/dev/null; then
    cat "${LOG_FILE}" >&2
    exit 1
  fi
  sleep 1
done

curl -fsS "${BASE_URL}/healthz" >/dev/null

node - "${BASE_URL}" "${FIXTURE}" <<'NODE'
void (async () => {
const [baseUrl, fixture] = process.argv.slice(2);

const response = await fetch(`${baseUrl}/api/containers`);
if (!response.ok) {
  throw new Error(`GET /api/containers failed: ${response.status}`);
}

const payload = await response.json();
const containers = payload.containers ?? [];
const fixtureContainer = containers.find((container) => container.localFolder === fixture);

if (!fixtureContainer) {
  console.error(JSON.stringify(containers, null, 2));
  throw new Error(`fixture devcontainer not discovered for ${fixture}`);
}

if (fixtureContainer.containerUser !== "vscode") {
  console.error(JSON.stringify(fixtureContainer, null, 2));
  throw new Error(`expected containerUser vscode, got ${fixtureContainer.containerUser}`);
}

if (fixtureContainer.rule !== "default") {
  console.error(JSON.stringify(fixtureContainer, null, 2));
  throw new Error(`expected default rule, got ${fixtureContainer.rule}`);
}

if (!fixtureContainer.labels?.["devcontainer.local_folder"]) {
  console.error(JSON.stringify(fixtureContainer, null, 2));
  throw new Error("expected devcontainer.local_folder label");
}

console.log(`discovered ${fixtureContainer.name} (${fixtureContainer.shortId})`);
})().catch((error) => {
  console.error(error);
  process.exit(1);
});
NODE
