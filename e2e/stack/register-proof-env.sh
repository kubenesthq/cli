#!/usr/bin/env bash
# register-proof-env.sh — stand up a REAL control plane + REAL Gitea for the
# kn-7k8 register-stage idempotency proof, then tear it down.
#
# WHY A DEDICATED STACK
#   AGENTS.md rule §1 forbids mocks: the proof has to register against a real
#   backend talking to a real Gitea. It does NOT reuse kubenest-backend's shared
#   docker-compose stack, because configuring GITEA_* there would change how
#   POST /clusters/{id}/agent-credentials behaves for every other agent working
#   in this workspace — the mint 502s when Gitea is configured but unreachable.
#   Everything here is namespaced kn7k8-proof-* and on non-default ports.
#
# USAGE
#   ./e2e/stack/register-proof-env.sh up      # prints the exports to source
#   ./e2e/stack/register-proof-env.sh down
#   ./e2e/stack/register-proof-env.sh status
#
# After `up`:
#   source /tmp/kn7k8-proof.env
#   go test -tags e2e -v -run TestRegisterStage ./e2e/

set -euo pipefail

CLI_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BACKEND="$(cd "$CLI_ROOT/../kubenest-backend" && pwd)"

PREFIX=kn7k8-proof
PG_PORT=${PG_PORT:-55432}
GITEA_PORT=${GITEA_PORT:-3010}
API_PORT=${API_PORT:-8010}
GITEA_ADMIN=kubenest
GITEA_PASSWORD=${GITEA_PASSWORD:-proof-only-not-a-secret}
ENV_FILE=/tmp/${PREFIX}.env
PID_FILE=/tmp/${PREFIX}.uvicorn.pid
LOG_FILE=/tmp/${PREFIX}.uvicorn.log

# Gitea's deploy-key grace window. 0 makes rotation observable in one run:
# the superseded key is deleted immediately instead of 24h later. The proof
# exercises both settings — see e2e/register_idempotency_test.go.
GRACE=${GITEA_DEPLOY_KEY_GRACE_SECONDS:-0}

log() { printf '  %s\n' "$*" >&2; }

wait_for() {
  local label="$1" url="$2" attempts="${3:-60}" i
  for ((i = 1; i <= attempts; i++)); do
    if curl -fsS -o /dev/null "$url" 2>/dev/null; then
      log "$label ready"
      return 0
    fi
    sleep 1
  done
  echo "$label never became ready at $url" >&2
  return 1
}

up() {
  command -v docker >/dev/null || { echo "docker is required" >&2; exit 2; }
  [ -x "$BACKEND/.venv/bin/python" ] || {
    echo "kubenest-backend/.venv is missing: pip install -r requirements-dev.txt first" >&2
    exit 2
  }

  log "postgres on :$PG_PORT"
  docker rm -f "${PREFIX}-pg" >/dev/null 2>&1 || true
  docker run -d --name "${PREFIX}-pg" \
    -e POSTGRES_USER=kubenest -e POSTGRES_PASSWORD=kubenest -e POSTGRES_DB=kubenest \
    -p "${PG_PORT}:5432" postgres:16 >/dev/null

  log "redis on :$((PG_PORT + 1))"
  docker rm -f "${PREFIX}-redis" >/dev/null 2>&1 || true
  docker run -d --name "${PREFIX}-redis" -p "$((PG_PORT + 1)):6379" redis:alpine >/dev/null

  # Gitea 1.27.0 is the app version the pinned chart (12.7.0) ships — op3
  # charts/kubenest-operator/Chart.yaml. Same server the broker will meet in
  # a real control plane.
  log "gitea 1.27.0 on :$GITEA_PORT"
  docker rm -f "${PREFIX}-gitea" >/dev/null 2>&1 || true
  docker run -d --name "${PREFIX}-gitea" \
    -e GITEA__security__INSTALL_LOCK=true \
    -e GITEA__server__ROOT_URL="http://localhost:${GITEA_PORT}/" \
    -e GITEA__server__SSH_PORT=22 \
    -e GITEA__database__DB_TYPE=sqlite3 \
    -p "${GITEA_PORT}:3000" gitea/gitea:1.27.0 >/dev/null

  wait_for "gitea" "http://localhost:${GITEA_PORT}/api/v1/version" 120

  log "gitea admin user + api token"
  docker exec -u git "${PREFIX}-gitea" gitea admin user create \
    --username "$GITEA_ADMIN" --password "$GITEA_PASSWORD" \
    --email "$GITEA_ADMIN@proof.local" --admin --must-change-password=false >/dev/null 2>&1 || true

  local gitea_token
  gitea_token=$(curl -fsS -X POST \
    -H 'Content-Type: application/json' \
    -u "${GITEA_ADMIN}:${GITEA_PASSWORD}" \
    -d '{"name":"kn7k8-proof-'"$RANDOM"'","scopes":["write:repository","write:user","write:admin"]}' \
    "http://localhost:${GITEA_PORT}/api/v1/users/${GITEA_ADMIN}/tokens" | sed -n 's/.*"sha1":"\([^"]*\)".*/\1/p')
  [ -n "$gitea_token" ] || { echo "could not mint a Gitea API token" >&2; exit 1; }

  log "waiting for postgres"
  local i
  for ((i = 1; i <= 60; i++)); do
    docker exec "${PREFIX}-pg" pg_isready -U kubenest >/dev/null 2>&1 && break
    sleep 1
  done

  cat > "$ENV_FILE" <<EOF
# kn-7k8 register-stage proof environment. Sourced by the e2e test.
export KUBENEST_PROOF_API=http://localhost:${API_PORT}
export TEST_GITEA_URL=http://localhost:${GITEA_PORT}
export TEST_GITEA_TOKEN=${gitea_token}
export TEST_GITEA_OWNER=${GITEA_ADMIN}
export KUBENEST_PROOF_GRACE_SECONDS=${GRACE}
export KN7K8_PROOF_ENV=${ENV_FILE}
EOF

  log "migrations + backend on :$API_PORT"
  (
    cd "$BACKEND"
    export POSTGRES_USER=kubenest POSTGRES_PASSWORD=kubenest \
      POSTGRES_SERVER=localhost POSTGRES_PORT="$PG_PORT" POSTGRES_DB=kubenest \
      REDIS_CACHE_HOST=localhost REDIS_CACHE_PORT=$((PG_PORT + 1)) \
      REDIS_QUEUE_HOST=localhost REDIS_QUEUE_PORT=$((PG_PORT + 1)) \
      REDIS_RATE_LIMIT_HOST=localhost REDIS_RATE_LIMIT_PORT=$((PG_PORT + 1)) \
      GITEA_URL="http://localhost:${GITEA_PORT}" \
      GITEA_USER="$GITEA_ADMIN" \
      GITEA_TOKEN="$gitea_token" \
      GITEA_DEPLOY_KEY_GRACE_SECONDS="$GRACE"
    .venv/bin/alembic upgrade head >>"$LOG_FILE" 2>&1
    nohup .venv/bin/uvicorn app.main:app --host 127.0.0.1 --port "$API_PORT" \
      >>"$LOG_FILE" 2>&1 &
    echo $! > "$PID_FILE"
  )

  wait_for "backend" "http://localhost:${API_PORT}/api/v1/health" 90 || {
    tail -40 "$LOG_FILE" >&2
    exit 1
  }

  seed

  log ""
  log "ready. source $ENV_FILE"
  cat "$ENV_FILE"
}

# seed logs in as the admin the backend seeds on first start, finds the default
# organization, and mints a REAL CLI token with the four installer scopes —
# through the API, so the proof exercises kn-odqp's mint as well.
seed() {
  local api="http://localhost:${API_PORT}"
  local jwt org cli

  jwt=$(curl -fsS -X POST "$api/api/v1/login" \
    -H 'Content-Type: application/x-www-form-urlencoded' \
    --data-urlencode "username=${ADMIN_EMAIL:-admin@admin.com}" \
    --data-urlencode "password=${ADMIN_PASSWORD:-!Ch4ng3Th1sP4ssW0rd!}" \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)["access_token"])')
  [ -n "$jwt" ] || { echo "admin login failed" >&2; exit 1; }

  org=$(curl -fsS "$api/api/v1/orgs" -H "Authorization: Bearer $jwt" \
    | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d[0]["id"] if d else "")')
  [ -n "$org" ] || { echo "no organization seeded" >&2; exit 1; }

  # Bound to that org on purpose: it is the shape a channel partner's token
  # takes, and it exercises the org-binding path on every stage-2 route.
  cli=$(curl -fsS -X POST "$api/api/v1/user/me/cli-tokens" \
    -H "Authorization: Bearer $jwt" -H 'Content-Type: application/json' \
    -d "{\"name\":\"kn7k8 proof installer\",
         \"scopes\":[\"clusters:read\",\"clusters:register\",\"bundles:read\",\"install:report\"],
         \"ttl_days\":1,\"org_id\":\"$org\"}" \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')
  [ -n "$cli" ] || { echo "minting the CLI token failed" >&2; exit 1; }

  cat >> "$ENV_FILE" <<EOF
export KUBENEST_PROOF_TOKEN=${cli}
export KUBENEST_PROOF_ORG=${org}
EOF
  log "seeded org ${org} and a knp_ token with the four installer scopes"
}

down() {
  if [ -f "$PID_FILE" ]; then
    kill "$(cat "$PID_FILE")" 2>/dev/null || true
    rm -f "$PID_FILE"
  fi
  # uvicorn --reload spawns children; catch strays bound to our port only.
  pkill -f "uvicorn app.main:app --host 127.0.0.1 --port ${API_PORT}" 2>/dev/null || true
  docker rm -f "${PREFIX}-pg" "${PREFIX}-redis" "${PREFIX}-gitea" >/dev/null 2>&1 || true
  rm -f "$ENV_FILE"
  log "torn down"
}

status() {
  docker ps --filter "name=${PREFIX}-" --format '  {{.Names}}\t{{.Status}}\t{{.Ports}}' >&2
  curl -fsS -o /dev/null "http://localhost:${API_PORT}/api/v1/health" 2>/dev/null \
    && log "backend :$API_PORT up" || log "backend :$API_PORT down"
}

case "${1:-}" in
  up) up ;;
  down) down ;;
  status) status ;;
  *) echo "usage: $0 up|down|status" >&2; exit 2 ;;
esac
