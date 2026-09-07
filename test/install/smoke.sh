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
#   THE FIXTURE'S SHAPE IS NOT THIS SCRIPT'S TO INVENT, and it was, once. The
#   installer downloaded checksums.txt.sigstore.json while build.yml signed
#   into a detached .sig/.pem pair, so install.sh could not install any release
#   this repository cut — and this test passed throughout, because it built the
#   bundle itself. contract_test.sh now compares install.sh against build.yml
#   directly and runs FIRST, below, so the fixture cannot drift from the
#   workflow again without something going red.
#
# WHAT THIS TEST DOES NOT COVER, stated rather than left silent:
#   REAL KEYLESS VERIFICATION. Keyless needs a Fulcio certificate chained to
#   the Sigstore public-good root and a Rekor entry, which cannot be minted
#   without a GitHub Actions OIDC token — so the happy path here uses a pinned
#   public key (install.sh's KUBENEST_COSIGN_PUBKEY mode) and NOT the keyless
#   branch every real user takes. Case 8 drives install.sh with no pinned key
#   so the keyless branch is at least entered and its cosign flags are proven
#   to parse, but it cannot prove a genuine Fulcio identity matches
#   KEYLESS_IDENTITY_REGEXP. That gap closes only against a real signed
#   release. Until then the identity regexp is covered statically by
#   contract_test.sh, which asserts it is identical to build.yml's, anchored,
#   and restricted to release tags.
#
# Requirements: docker, go, cosign, python3.
# Run:  bash test/install/smoke.sh

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
INSTALL_SH="${REPO_ROOT}/install.sh"
PORT="${SMOKE_PORT:-8123}"
BASE_IMAGE="kubenest-install-smoke-base-nqei"

log()  { printf '\033[0;36m[smoke]\033[0m %s\n' "$*"; }
pass() { printf '\033[0;32m[smoke] PASS:\033[0m %s\n' "$*"; }
fail() { printf '\033[0;31m[smoke] FAIL:\033[0m %s\n' "$*" >&2; exit 1; }

# --- prerequisites ---------------------------------------------------------
for tool in docker go python3 curl; do
  command -v "$tool" >/dev/null 2>&1 || fail "missing required tool: $tool"
done
COSIGN_BIN="${SMOKE_COSIGN:-$(command -v cosign || true)}"
[ -n "$COSIGN_BIN" ] || fail "missing cosign (needed to sign the fixture release); set SMOKE_COSIGN to a binary"

# --- the fixture must match the workflow, checked before anything is built ---
log "checking the install.sh <-> build.yml release contract"
sh "${REPO_ROOT}/test/install/contract_test.sh" \
  || fail "install.sh and build.yml do not describe the same release; fix that before trusting anything below"

# The fixture must be signed in the format PRODUCTION emits, or this test
# exercises an artifact shape that will never exist. build.yml pins cosign to
# install.sh's COSIGN_VERSION and signs with plain --bundle, which on v3 means
# the new Sigstore bundle; cosign v2's --bundle defaults to the legacy format
# and needs --new-bundle-format to match. (v3's verify-blob reads both, so a
# v2-signed fixture was never a false green — but it was not production's
# artifact either.)
PINNED_COSIGN="$(sed -n 's/^COSIGN_VERSION="\(.*\)"$/\1/p' "${REPO_ROOT}/install.sh")"
COSIGN_VER="$("$COSIGN_BIN" version 2>/dev/null | sed -n 's/^ *GitVersion: *//p')"
BUNDLE_FLAGS=""
case "$COSIGN_VER" in
  v3.*) : ;;
  v2.*) BUNDLE_FLAGS="--new-bundle-format" ;;
  *)    fail "cannot read cosign's version from ${COSIGN_BIN} (got '${COSIGN_VER}')" ;;
esac
if [ "$COSIGN_VER" != "$PINNED_COSIGN" ]; then
  log "NOTE: fixture signed with cosign ${COSIGN_VER}; install.sh pins ${PINNED_COSIGN}."
  log "      Forcing the new bundle format so the artifact matches what build.yml emits."
  log "      Set SMOKE_COSIGN to a ${PINNED_COSIGN} binary to test the exact production signer."
fi

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
 && useradd --create-home --uid 10001 --shell /bin/sh kubenest \
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
  # shellcheck disable=SC2086 # BUNDLE_FLAGS is deliberately word-split: empty, or one flag
  COSIGN_PASSWORD=smoketest "$COSIGN_BIN" sign-blob --yes $BUNDLE_FLAGS \
    --key cosign.key --bundle checksums.txt.sigstore.json checksums.txt >/dev/null 2>&1
)
[ -s "${RELEASE_DIR}/checksums.txt.sigstore.json" ] || fail "bundle was not produced"

# The exact bytes checksums.txt was computed over. The tamper case restores
# from this rather than re-running `go build`: the rebuild is not byte-identical
# here, so every later case was failing its CHECKSUM instead of the thing it
# claimed to test — the wrong-key case in particular never reached the
# signature check at all.
cp "${RELEASE_DIR}/${ASSET}" "${WORK}/asset.pristine"

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
INSTALL_LOCAL_CMD='curl -fsSL "http://127.0.0.1:'"$PORT"'/install.sh" | sh -s -- --install-dir ~/.local/bin'

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

# --- 5. printed no-sudo remedy: a missing user directory must work ---------
# The error recommends --install-dir ~/.local/bin. Exercise that exact remedy
# on a fresh Ubuntu user without sudo, and require the directory to start
# absent: otherwise this can regress to the self-referential error again.
log "non-root no-sudo path: follow the printed --install-dir remedy into a missing directory"
NONROOT_REMEDY_BODY='set -eu
  command -v sudo >/dev/null 2>&1 && { echo "sudo must be absent"; exit 93; }
  test ! -e "$HOME/.local/bin" || { echo "target directory already exists"; exit 94; }
  set +e
  out=$( '"${INSTALL_CMD}"' 2>&1 )
  rc=$?
  set -e
  printf "%s\\n" "$out"
  [ "$rc" -ne 0 ] || { echo "default install unexpectedly succeeded"; exit 95; }
  case "$out" in
    *"remedy: re-run with --install-dir ~/.local/bin"*) : ;;
    *) echo "installer did not print the exact no-sudo remedy"; exit 96 ;;
  esac
  test ! -e "$HOME/.local/bin" || { echo "default install created the remedy directory"; exit 97; }
  '"${INSTALL_LOCAL_CMD}"'
  test -x "$HOME/.local/bin/kubenest" || { echo "remedy did not install the binary"; exit 98; }
  "$HOME/.local/bin/kubenest" --version | grep -F '"${VERSION}"' \
    || { echo "remedy installed the wrong version"; exit 99; }
'
set +e
docker run --rm --network host --user 10001:10001 \
  -v "${RELEASE_DIR}:/fixture:ro" \
  -v "${COSIGN_BIN}:/tools/cosign:ro" \
  -e HOME=/home/kubenest \
  -e KUBENEST_RELEASE_BASE="http://127.0.0.1:${PORT}" \
  -e KUBENEST_RELEASE_API="http://127.0.0.1:${PORT}/api" \
  -e KUBENEST_VERSION="${VERSION}" \
  -e KUBENEST_COSIGN=/tools/cosign \
  -e KUBENEST_COSIGN_PUBKEY=/fixture/cosign.pub \
  "$BASE_IMAGE" bash -c "${NONROOT_REMEDY_BODY}"
rc=$?
set -e
case "$rc" in
  0) pass "printed non-root remedy created ~/.local/bin and installed without sudo" ;;
  93) fail "non-root remedy image unexpectedly has sudo" ;;
  94) fail "non-root remedy target was not a clean missing directory" ;;
  95) fail "non-root default install unexpectedly succeeded" ;;
  96) fail "installer remedy text drifted from the executable command" ;;
  97) fail "non-root default install mutated the remedy directory" ;;
  98) fail "printed non-root remedy did not install a binary" ;;
  99) fail "printed non-root remedy installed the wrong version" ;;
  *) fail "printed non-root remedy container exited $rc" ;;
esac

# Negative-path container body: run the installer, require it to fail, require
# that NOTHING was installed, and require that it failed for the REASON this
# case is about. That last assertion is not decoration — without it a case can
# pass on an unrelated earlier failure and still read green, which is exactly
# what the wrong-key case did once the tamper case stopped restoring the signed
# bytes. $EXPECT is supplied per case via docker -e.
# Exits: 88 installer succeeded, 89 aborted but installed anyway, 92 failed for
# a different reason than $EXPECT.
# Double-quoted so INSTALL_CMD expands now; \$? and \$rc stay literal until
# the container's shell runs them.
NEG_BODY="out=\$( ${INSTALL_CMD} 2>&1 ); rc=\$?
printf '%s\\n' \"\$out\"
[ \"\$rc\" -ne 0 ] || { echo 'installer unexpectedly succeeded'; exit 88; }
test ! -e /usr/local/bin/kubenest || { echo 'binary installed despite abort'; exit 89; }
case \"\$out\" in
  *\"\$EXPECT\"*) : ;;
  *) echo \"aborted, but not because of: \$EXPECT\"; exit 92 ;;
esac"

# neg_fail turns a container exit code into a sentence.
neg_fail() {
  case "$2" in
    88) fail "$1: the installer SUCCEEDED when it had to refuse" ;;
    89) fail "$1: aborted but installed a binary anyway" ;;
    92) fail "$1: aborted for the wrong reason — see the output above" ;;
    *)  fail "$1: container exited $2" ;;
  esac
}

# --- 5. negative: tampered binary must abort, leaving nothing behind -------
log "negative path: corrupted binary must abort and install nothing"
printf 'X' | dd of="${RELEASE_DIR}/${ASSET}" bs=1 seek=100 conv=notrunc status=none
set +e
run_container -e EXPECT="SHA-256 mismatch" -e KUBENEST_COSIGN_PUBKEY=/fixture/cosign.pub -- bash -c "${NEG_BODY}"
rc=$?
set -e
[ "$rc" -eq 0 ] || neg_fail "tampered binary" "$rc"
pass "tampered binary aborted at the checksum and installed nothing"
# Restore the exact signed bytes. NOT a rebuild — see asset.pristine above.
cp "${WORK}/asset.pristine" "${RELEASE_DIR}/${ASSET}"

# --- 6. negative: wrong public key must abort ------------------------------
log "negative path: wrong public key must abort and install nothing"
set +e
run_container -e EXPECT="signature verification FAILED" -e KUBENEST_COSIGN_PUBKEY=/fixture/wrong.pub -- bash -c "${NEG_BODY}"
rc=$?
set -e
[ "$rc" -eq 0 ] || neg_fail "wrong public key" "$rc"
pass "wrong-key signature aborted at the signature check and installed nothing"

# --- 7. negative: a release in the shape build.yml produced BEFORE kn-s9ck ---
# checksums.txt plus a DETACHED signature and certificate, and no Sigstore
# bundle. That is what this repository's release workflow emitted while
# install.sh was downloading checksums.txt.sigstore.json — the installer could
# not install any release this repo cut, and this test passed anyway because it
# built the bundle itself. Served as its own release directory rather than by
# mutating the good one, so the happy-path fixture stays untouched.
log "negative path: pre-kn-s9ck release layout (detached .sig/.pem, no bundle) must abort"
OLD_VERSION="v9.9.8-prefix"
OLD_DIR="${WORK}/serve/download/${OLD_VERSION}"
mkdir -p "$OLD_DIR"
cp "${RELEASE_DIR}/${ASSET}" "${OLD_DIR}/kubenest-${OLD_VERSION}-linux-amd64"
(
  cd "$OLD_DIR"
  sha256sum "kubenest-${OLD_VERSION}-linux-amd64" > checksums.txt
  printf 'MEUCIQD-not-a-real-detached-signature\n' > checksums.txt.sig
  printf -- '-----BEGIN CERTIFICATE-----\nnot-a-real-certificate\n-----END CERTIFICATE-----\n' > checksums.txt.pem
)
[ -e "${OLD_DIR}/checksums.txt.sigstore.json" ] && fail "the pre-fix fixture must NOT contain a bundle"
set +e
run_container -e EXPECT="could not download the Sigstore bundle" \
  -e KUBENEST_VERSION="${OLD_VERSION}" -e KUBENEST_COSIGN_PUBKEY=/fixture/cosign.pub -- bash -c "${NEG_BODY}"
rc=$?
set -e
[ "$rc" -eq 0 ] || neg_fail "pre-kn-s9ck release layout" "$rc"
pass "a release with a detached signature and no bundle aborted and installed nothing"

# --- 8. the KEYLESS branch is entered and its flags parse -------------------
# Every case above pins a public key, so install.sh takes the
# KUBENEST_COSIGN_PUBKEY branch and the keyless branch — the DEFAULT for every
# real user, and the one carrying the identity pinning — never runs. Drop the
# pinned key and it does.
#
# What this proves: the keyless branch is reached, and cosign accepts
# --certificate-identity-regexp / --certificate-oidc-issuer as written (a typo,
# or a flag renamed by a cosign upgrade, would surface as a usage error rather
# than a verification failure). What it CANNOT prove: that a genuine Fulcio
# identity matches KEYLESS_IDENTITY_REGEXP — the fixture is key-signed, because
# minting a Fulcio certificate needs a GitHub Actions OIDC token. Verification
# MUST fail here; the assertion is on HOW it fails. See the header.
log "keyless path: entered, flags parse, and it fails on verification rather than usage"
KEYLESS_BODY="out=\$( ${INSTALL_CMD} 2>&1 ); rc=\$?
printf '%s\\n' \"\$out\"
[ \"\$rc\" -ne 0 ] || { echo 'keyless verification unexpectedly succeeded'; exit 88; }
test ! -e /usr/local/bin/kubenest || { echo 'binary installed despite abort'; exit 89; }
case \"\$out\" in
  *'unknown flag'*|*'unknown shorthand'*|*'unknown command'*|*'Error: accepts'*)
    echo 'cosign rejected the keyless FLAGS, not the signature'; exit 90 ;;
esac
case \"\$out\" in
  *'signature verification FAILED'*) : ;;
  *) echo 'the failure did not come from the signature check'; exit 91 ;;
esac"
set +e
run_container -- bash -c "${KEYLESS_BODY}"
rc=$?
set -e
case "$rc" in
  0)  pass "keyless branch entered; cosign accepted the identity flags and failed on the signature" ;;
  88) fail "keyless verification SUCCEEDED against a key-signed fixture — install.sh is not verifying identity" ;;
  89) fail "keyless verification aborted but a binary was installed anyway" ;;
  90) fail "cosign rejected install.sh's keyless FLAGS — --certificate-identity-regexp / --certificate-oidc-issuer do not match this cosign" ;;
  91) fail "the keyless run failed somewhere other than the signature check; read the output above" ;;
  *)  fail "keyless case exited ${rc}" ;;
esac

printf '\n\033[0;32m[smoke] ALL CHECKS PASSED\033[0m\n'
printf '\033[1;33m[smoke] NOT COVERED: real keyless verification against a Fulcio identity — see the header.\033[0m\n'
