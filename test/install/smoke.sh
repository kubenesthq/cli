#!/usr/bin/env bash
# test/install/smoke.sh — container smoke test for ../../install.sh
#
# This exercises the REAL download -> verify -> install path end to end:
#   1. Builds a genuine linux/amd64 kubenest binary from this checkout.
#   2. Produces a release layout: the binary, checksums.txt, and a real
#      Sigstore bundle over checksums.txt signed with a locally generated key.
#   3. Serves that layout (and install.sh) over a local HTTP server.
#   4. Runs install.sh inside an Ubuntu 24.04 container (whose /bin/sh is
#      dash, the documented target) via the documented `curl | sh` form,
#      pointed at the local server, with a pinned public key for signature
#      verification.
#   5. Asserts the binary lands and runs, and that tampering aborts.
#
# WHY A FIXTURE INSTEAD OF A PUBLISHED RELEASE:
#   The only published release (v1.0.6, 2025) predates Sigstore bundle signing
#   and ships no signature, so install.sh correctly refuses it. There is no
#   usable signed public release to test against until one is cut. So this
#   test builds a real signed release locally and serves it — every step the
#   installer performs (HTTP fetch, SHA-256 check, cosign bundle verify,
#   install) runs for real, against real artifacts. This is NOT a mock: the
#   crypto is real. It is a fixture only in that it is not hosted on GitHub
#   under a live tag. See the bead before trusting a "green" here.
#
# Requirements: docker, go, cosign (v2 or v3), python3.
# Run:  bash test/install/smoke.sh

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
INSTALL_SH="${REPO_ROOT}/install.sh"
PORT="${SMOKE_PORT:-8123}"
BASE_IMAGE="kubenest-install-smoke-base"

log()  { printf '\033[0;36m[smoke]\033[0m %s\n' "$*"; }
pass() { printf '\033[0;32m[smoke] PASS:\033[0m %s\n' "$*"; }
fail() { printf '\033[0;31m[smoke] FAIL:\033[0m %s\n' "$*" >&2; exit 1; }

# --- prerequisites ---------------------------------------------------------
for tool in docker go python3 curl; do
  command -v "$tool" >/dev/null 2>&1 || fail "missing required tool: $tool"
done
COSIGN_BIN="$(command -v cosign || true)"
[ -n "$COSIGN_BIN" ] || fail "missing cosign (needed to sign the fixture release)"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/kubenest-smoke.XXXXXX")"
SERVER_PID=""
cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

# --- base image with curl (so install.sh has a downloader) -----------------
if ! docker image inspect "$BASE_IMAGE" >/dev/null 2>&1; then
  log "building base image ${BASE_IMAGE}"
  docker build -t "$BASE_IMAGE" - >/dev/null <<'EOF'
FROM ubuntu:24.04
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates \
 && rm -rf /var/lib/apt/lists/*
EOF
fi

# --- 1. build a real binary ------------------------------------------------
VERSION="v9.9.9-smoke"
ASSET="kubenest-${VERSION}-linux-amd64"
RELEASE_DIR="${WORK}/serve/download/${VERSION}"
mkdir -p "$RELEASE_DIR"

log "building ${ASSET} from this checkout"
(
  cd "$REPO_ROOT"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags "-s -w -X kubenest.io/cli/pkg/version.Version=${VERSION}" \
    -o "${RELEASE_DIR}/${ASSET}" ./cmd/kubenest
)

# --- 2. checksums + a real Sigstore bundle over them -----------------------
log "generating checksums.txt and signing it with cosign"
(
  cd "$RELEASE_DIR"
  sha256sum "$ASSET" > checksums.txt
  COSIGN_PASSWORD=smoketest "$COSIGN_BIN" generate-key-pair >/dev/null 2>&1
  COSIGN_PASSWORD=smoketest "$COSIGN_BIN" sign-blob --yes \
    --key cosign.key --bundle checksums.txt.sigstore.json checksums.txt >/dev/null 2>&1
)
[ -s "${RELEASE_DIR}/checksums.txt.sigstore.json" ] || fail "bundle was not produced"

# A wrong key, for the negative test.
( cd "$WORK" && COSIGN_PASSWORD=other "$COSIGN_BIN" generate-key-pair >/dev/null 2>&1 )
cp "$WORK/cosign.pub" "${RELEASE_DIR}/wrong.pub"

# Serve install.sh too, so the documented `curl | sh` form is what we test.
cp "$INSTALL_SH" "${WORK}/serve/install.sh"

# Also serve a "latest" API endpoint for version-resolution testing.
mkdir -p "${WORK}/serve/api/releases"
printf '{"tag_name": "%s"}\n' "$VERSION" > "${WORK}/serve/api/releases/latest"

# --- 3. serve it -----------------------------------------------------------
log "serving fixture release on 127.0.0.1:${PORT}"
# --directory keeps $! = the python PID (no subshell), so cleanup kills it.
python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "${WORK}/serve" >/dev/null 2>&1 &
SERVER_PID=$!
sleep 1
curl -fsS "http://127.0.0.1:${PORT}/download/${VERSION}/checksums.txt" >/dev/null \
  || fail "fixture server is not serving"

# --- container runner ------------------------------------------------------
# Runs install.sh in a fresh container; the installer is invoked exactly as
# documented: curl | sh. Extra docker args (e.g. -e KUBENEST_COSIGN_PUBKEY=...)
# come first, terminated by '--', then the container command:
#   run_container -e KUBENEST_COSIGN_PUBKEY=/fixture/cosign.pub -- bash -c '...'
run_container() {
  _extra=()
  while [ "$1" != "--" ]; do _extra+=("$1"); shift; done
  shift
  docker run --rm --network host \
    -v "${RELEASE_DIR}:/fixture:ro" \
    -v "${COSIGN_BIN}:/tools/cosign:ro" \
    -e KUBENEST_RELEASE_BASE="http://127.0.0.1:${PORT}" \
    -e KUBENEST_RELEASE_API="http://127.0.0.1:${PORT}/api" \
    -e KUBENEST_VERSION="${VERSION}" \
    -e KUBENEST_INSTALL_DIR=/usr/local/bin \
    -e KUBENEST_COSIGN=/tools/cosign \
    ${_extra[@]+"${_extra[@]}"} \
    "$BASE_IMAGE" "$@"
}

INSTALL_CMD='curl -fsSL "http://127.0.0.1:'"$PORT"'/install.sh" | sh'

# --- 4. happy path: verified download + install + idempotence --------------
log "happy path: curl | sh, verified install"
run_container -e KUBENEST_COSIGN_PUBKEY=/fixture/cosign.pub -- bash -c '
  set -e
  '"${INSTALL_CMD}"'
  test -x /usr/local/bin/kubenest || { echo "binary missing"; exit 1; }
  /usr/local/bin/kubenest --version | grep -F "'"${VERSION}"'" \
    || { echo "version mismatch"; exit 1; }
  echo "--- idempotent re-run ---"
  '"${INSTALL_CMD}"'
  echo "--- bash invocation (POSIX script must run under bash too) ---"
  curl -fsSL "http://127.0.0.1:'"$PORT"'/install.sh" | bash -s -- --force
' || fail "happy path did not install a verified binary"
pass "verified download installed and runs; re-run is a no-op"

# Negative-path container body: run the installer, require it to fail, and
# require that NOTHING was installed. Exits 88 if the installer succeeded,
# 89 if it aborted but left a binary behind.
# Double-quoted so INSTALL_CMD expands now; \$? and \$rc stay literal until
# the container's shell runs them.
NEG_BODY="rc=0
${INSTALL_CMD} >/dev/null 2>&1 || rc=\$?
[ \"\$rc\" -ne 0 ] || { echo 'installer unexpectedly succeeded'; exit 88; }
test ! -e /usr/local/bin/kubenest || { echo 'binary installed despite abort'; exit 89; }"

# --- 5. negative: tampered binary must abort, leaving nothing behind -------
log "negative path: corrupted binary must abort and install nothing"
printf 'X' | dd of="${RELEASE_DIR}/${ASSET}" bs=1 seek=100 conv=notrunc status=none
set +e
run_container -e KUBENEST_COSIGN_PUBKEY=/fixture/cosign.pub -- bash -c "${NEG_BODY}"
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "tampered binary not cleanly rejected (container exit ${rc}: 88=installed, 89=installed-despite-abort)"
pass "tampered binary aborted and installed nothing"
# restore the good binary for the next test
( cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags "-s -w -X kubenest.io/cli/pkg/version.Version=${VERSION}" \
    -o "${RELEASE_DIR}/${ASSET}" ./cmd/kubenest )

# --- 6. negative: wrong public key must abort ------------------------------
log "negative path: wrong public key must abort and install nothing"
set +e
run_container -e KUBENEST_COSIGN_PUBKEY=/fixture/wrong.pub -- bash -c "${NEG_BODY}"
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "wrong-key signature not cleanly rejected (container exit ${rc})"
pass "wrong-key signature aborted and installed nothing"

# --- 7. negative: unsigned release (no bundle) must abort ------------------
# This is the shape of every release published before Sigstore bundle signing
# (e.g. v1.0.6): checksums exist, no signature. The installer must refuse it.
log "negative path: release with no Sigstore bundle must abort"
mv "${RELEASE_DIR}/checksums.txt.sigstore.json" "${WORK}/bundle.aside"
set +e
run_container -e KUBENEST_COSIGN_PUBKEY=/fixture/cosign.pub -- bash -c "${NEG_BODY}"
rc=$?
set -e
mv "${WORK}/bundle.aside" "${RELEASE_DIR}/checksums.txt.sigstore.json"
[ "$rc" -eq 0 ] || fail "unsigned release not cleanly rejected (container exit ${rc})"
pass "unsigned release (no bundle) aborted and installed nothing"

printf '\n\033[0;32m[smoke] ALL CHECKS PASSED\033[0m\n'
