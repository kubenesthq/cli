#!/bin/sh
# install.sh — obtain and verify the kubenest CLI.
#
#   curl -fsSL https://get.kubenest.io | sh
#
# Installs the kubenest platform CLI onto the machine running it. The CLI is a
# single static binary; this script downloads the release asset for the host
# OS/arch, verifies it (SHA-256 checksum AND Sigstore signature), and places it
# on the PATH. Nothing is installed unless verification succeeds.
#
# This script is POSIX sh, not bash. It must run under dash (the /bin/sh on
# Ubuntu, the documented target), bash, busybox ash and macOS /bin/sh. That is
# enforced by shellcheck --shell=sh in CI and by a container smoke test. Do not
# add bashisms: no arrays, no [[ ]], no `local`, no pipefail, no process
# substitution, no `source`.
#
# Verification model (both checks run; a failure aborts before any install):
#   1. Integrity — the downloaded binary matches its SHA-256 entry in the
#      release's checksums.txt.
#   2. Authenticity — checksums.txt carries a Sigstore signature produced by
#      this repo's release workflow (.github/workflows/build.yml). By default
#      that is verified keyless against the GitHub Actions OIDC identity of the
#      workflow; set KUBENEST_COSIGN_PUBKEY to verify against a pinned public
#      key instead (air-gapped mirrors, or environments that pin a key).
#
# If cosign is not on PATH it is downloaded (pinned by SHA-256 below) into a
# temporary directory and used from there; it is never installed system-wide.
#
# Environment overrides (all optional):
#   KUBENEST_VERSION        exact release tag to install (default: latest)
#   KUBENEST_INSTALL_DIR    directory to install into (default: /usr/local/bin)
#   KUBENEST_RELEASE_BASE   release download base
#                           (default: https://github.com/kubenesthq/cli/releases)
#   KUBENEST_RELEASE_API    API base used to resolve "latest"
#                           (default: https://api.github.com/repos/kubenesthq/cli)
#   KUBENEST_COSIGN_PUBKEY  path to a public key; switches signature
#                           verification from keyless to pinned-key mode
#   KUBENEST_COSIGN         path to an existing cosign binary (skip download)
#
# Exit codes: 0 success (including already-installed no-op), non-zero any
# failure. On failure the script prints the remedy and installs nothing.

set -eu
umask 022

# ---------------------------------------------------------------------------
# Constants. The cosign pin is verified against sigstore's published
# checksums; bump COSIGN_VERSION and the hashes together.
# ---------------------------------------------------------------------------
REPO="kubenesthq/cli"
RELEASE_BASE="${KUBENEST_RELEASE_BASE:-https://github.com/${REPO}/releases}"
RELEASE_API="${KUBENEST_RELEASE_API:-https://api.github.com/repos/${REPO}}"
INSTALL_DIR="${KUBENEST_INSTALL_DIR:-/usr/local/bin}"
BIN_NAME="kubenest"

COSIGN_VERSION="v3.1.3"
COSIGN_SHA_LINUX_AMD64="4629c757b7618056f8ddd7e2625ae9fdd94c0372a65049520bc7d9df9efc7f71"
COSIGN_SHA_LINUX_ARM64="c5d324e091826b0d7a78eb16fef316450b4eb9aaec045611c08ba06f5e73220a"
COSIGN_SHA_DARWIN_AMD64="2347488e5d5b25336644024dfeca5601b190e91197a71a917bda44744aff106c"
COSIGN_SHA_DARWIN_ARM64="5cf948c2f4dfe59687bdd0b8523709067383e03982cc543475c8a7dc70e92a76"

# Keyless identity of the release workflow. Must match the OIDC subject GitHub
# Actions issues for .github/workflows/build.yml in this repo, and the issuer
# GitHub uses for Actions tokens.
#
# ANCHORED, AND TAG-ONLY, both deliberately. cosign matches this with Go's
# regexp, which is a substring match unless anchored. And `@.*` would accept a
# signature from build.yml running on ANY ref — that workflow is tag-triggered
# today, so the guarantee would live in its trigger config rather than in this
# check; add a workflow_dispatch some day and a branch build would verify as a
# release. `@refs/tags/v.*` makes the verification itself carry the rule.
#
# This string must stay identical to the one in the release notes of
# .github/workflows/build.yml. test/install/contract_test.sh asserts that.
KEYLESS_IDENTITY_REGEXP='^https://github\.com/kubenesthq/cli/\.github/workflows/build\.yml@refs/tags/v.*$'
KEYLESS_OIDC_ISSUER='https://token.actions.githubusercontent.com'

VERSION="${KUBENEST_VERSION:-}"
QUIET=0
FORCE=0

# ---------------------------------------------------------------------------
# Logging. err() is never suppressed by --quiet.
# ---------------------------------------------------------------------------
info() { [ "$QUIET" -eq 1 ] || printf '\033[0;36m-> %s\033[0m\n' "$*"; }
ok()   { [ "$QUIET" -eq 1 ] || printf '\033[0;32m   %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m!! %s\033[0m\n' "$*" >&2; }
err()  { printf '\033[0;31mERROR: %s\033[0m\n' "$*" >&2; }

die() {
  err "$1"
  [ $# -gt 1 ] && err "remedy: $2"
  exit 1
}

usage() {
  cat <<'EOF'
kubenest installer

Usage:
  curl -fsSL https://get.kubenest.io | sh
  curl -fsSL https://get.kubenest.io | sh -s -- --version v1.2.3

Options:
  --version TAG     install a specific release tag (default: latest)
  --install-dir DIR install into DIR (default: /usr/local/bin,
                    or $KUBENEST_INSTALL_DIR)
  --force           reinstall even if the same version is already present
  --quiet           only print warnings and errors
  -h, --help        show this help

Environment:
  KUBENEST_VERSION, KUBENEST_INSTALL_DIR, KUBENEST_RELEASE_BASE,
  KUBENEST_RELEASE_API, KUBENEST_COSIGN_PUBKEY, KUBENEST_COSIGN
EOF
}

# ---------------------------------------------------------------------------
# Argument parsing. Invoked both as `./install.sh --flag` and as
# `curl ... | sh -s -- --flag`, so $@ carries the flags either way.
# ---------------------------------------------------------------------------
while [ $# -gt 0 ]; do
  case "$1" in
    --version)     [ $# -ge 2 ] || die "--version needs a value"; VERSION="$2"; shift 2 ;;
    --version=*)   VERSION="${1#--version=}"; shift ;;
    --install-dir) [ $# -ge 2 ] || die "--install-dir needs a value"; INSTALL_DIR="$2"; shift 2 ;;
    --install-dir=*) INSTALL_DIR="${1#--install-dir=}"; shift ;;
    --force)       FORCE=1; shift ;;
    --quiet)       QUIET=1; shift ;;
    -h|--help)     usage; exit 0 ;;
    *)             die "unknown argument: $1" "run with --help" ;;
  esac
done

# ---------------------------------------------------------------------------
# Temp workspace, cleaned on any exit.
# ---------------------------------------------------------------------------
WORKDIR=""
cleanup() { [ -n "$WORKDIR" ] && rm -rf "$WORKDIR"; }
trap cleanup EXIT INT TERM
WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/kubenest-install.XXXXXX")" \
  || die "cannot create a temporary directory"

# ---------------------------------------------------------------------------
# A downloader that works with curl or wget. fetch URL DEST
# ---------------------------------------------------------------------------
fetch() {
  _f_url="$1"; _f_dest="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --retry 3 --retry-delay 2 -o "$_f_dest" "$_f_url"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$_f_dest" "$_f_url"
  else
    die "need curl or wget to download" "install curl or wget and re-run"
  fi
}

# Print the SHA-256 of a file, hex only. No pipelines: POSIX sh has no
# pipefail, so a failing hash tool behind a pipe could be masked. Each tool's
# output is captured whole and the hash is extracted by parameter expansion.
sha256_of() {
  _h_out=""
  if command -v sha256sum >/dev/null 2>&1; then
    _h_out="$(sha256sum "$1")" || die "sha256sum failed on $1"
    printf '%s\n' "${_h_out%% *}"
  elif command -v shasum >/dev/null 2>&1; then
    _h_out="$(shasum -a 256 "$1")" || die "shasum failed on $1"
    printf '%s\n' "${_h_out%% *}"
  elif command -v openssl >/dev/null 2>&1; then
    # "SHA2-256(file)= <hash>" (newer) or "SHA256(file)= <hash>" (older)
    _h_out="$(openssl dgst -sha256 "$1")" || die "openssl failed on $1"
    _h_out="${_h_out##*= }"
    printf '%s\n' "${_h_out}"
  else
    die "need sha256sum, shasum or openssl to verify the download" \
        "install one of them and re-run"
  fi
}

# ---------------------------------------------------------------------------
# Platform detection -> GOOS/GOARCH used by the release workflow's asset
# names: kubenest-<tag>-<os>-<arch>[.exe]. See .github/workflows/build.yml.
# ---------------------------------------------------------------------------
detect_platform() {
  # No pipeline (no `uname | tr`): POSIX sh lacks pipefail, so detection must
  # not be able to mask a failure. case matches the spellings uname emits.
  _os="$(uname -s)" || die "uname failed; cannot detect the operating system"
  _machine="$(uname -m)" || die "uname failed; cannot detect the architecture"

  case "$_os" in
    Linux|linux)   OS="linux" ;;
    Darwin|darwin) OS="darwin" ;;
    MINGW*|MSYS*|CYGWIN*|Windows_NT)
      die "Windows is not supported by this installer" \
          "download the .exe from https://github.com/${REPO}/releases and verify it per the release notes"
      ;;
    *)
      die "unsupported operating system: $_os" \
          "the CLI ships for Linux and macOS; build from source for other systems"
      ;;
  esac

  case "$_machine" in
    x86_64|amd64)  ARCH="amd64" ;;
    arm64|aarch64) ARCH="arm64" ;;
    *)
      die "unsupported architecture: $_machine" \
          "the CLI ships for amd64 and arm64"
      ;;
  esac

  info "detected platform ${OS}/${ARCH}"
}

# ---------------------------------------------------------------------------
# Resolve which release to install. Uses $VERSION if set, else asks the API
# for the latest tag.
# ---------------------------------------------------------------------------
resolve_version() {
  if [ -n "$VERSION" ]; then
    info "installing pinned version ${VERSION}"
    return 0
  fi

  info "resolving latest release"
  _latest_json="${WORKDIR}/latest.json"
  fetch "${RELEASE_API}/releases/latest" "$_latest_json" \
    || die "could not reach the release API at ${RELEASE_API}/releases/latest" \
           "check your network, or pin a version with --version"

  # Extract "tag_name": "..." without assuming jq is present. Single awk, no
  # pipeline (POSIX sh has no pipefail; a masked failure must be impossible).
  VERSION="$(awk -F'"' '/"tag_name"[[:space:]]*:/ { print $4; exit }' "$_latest_json")" \
    || die "could not read the latest-release response"
  [ -n "$VERSION" ] || die "could not determine the latest release tag" \
                           "pin a version with --version"
  info "latest release is ${VERSION}"
}

# ---------------------------------------------------------------------------
# Idempotence: if the same version is already installed, stop here unless
# --force. Runs the installed binary to read its version rather than trusting
# a filename.
# ---------------------------------------------------------------------------
already_installed() {
  _target="${INSTALL_DIR}/${BIN_NAME}"
  [ -x "$_target" ] || return 1
  _have="$( "$_target" --version 2>/dev/null || true )"
  case "$_have" in
    *"$VERSION"*)
      if [ "$FORCE" -eq 1 ]; then
        info "${BIN_NAME} ${VERSION} is already installed; --force given, reinstalling"
        return 1
      fi
      ok "${BIN_NAME} ${VERSION} is already installed at ${_target}; nothing to do"
      info "re-run with --force to reinstall"
      return 0
      ;;
    *)
      info "found an existing install (${_have:-no version}); replacing it"
      return 1
      ;;
  esac
}

# ---------------------------------------------------------------------------
# Obtain a cosign binary: use $KUBENEST_COSIGN if set, a cosign already on
# PATH if present, else download the pinned release into $WORKDIR and verify
# its SHA-256 before use. Echoes the path to the cosign binary.
# ---------------------------------------------------------------------------
ensure_cosign() {
  if [ -n "${KUBENEST_COSIGN:-}" ]; then
    [ -x "$KUBENEST_COSIGN" ] || die "KUBENEST_COSIGN is not an executable: $KUBENEST_COSIGN"
    COSIGN="$KUBENEST_COSIGN"
    return 0
  fi
  if command -v cosign >/dev/null 2>&1; then
    COSIGN="cosign"
    info "using cosign already on PATH: $(command -v cosign)"
    return 0
  fi

  info "cosign not found; downloading pinned ${COSIGN_VERSION} to verify the release"
  _c_asset="cosign-${OS}-${ARCH}"
  case "${OS}-${ARCH}" in
    linux-amd64)  _c_sha="$COSIGN_SHA_LINUX_AMD64" ;;
    linux-arm64)  _c_sha="$COSIGN_SHA_LINUX_ARM64" ;;
    darwin-amd64) _c_sha="$COSIGN_SHA_DARWIN_AMD64" ;;
    darwin-arm64) _c_sha="$COSIGN_SHA_DARWIN_ARM64" ;;
    *) die "no pinned cosign for ${OS}-${ARCH}" ;;
  esac

  _c_url="https://github.com/sigstore/cosign/releases/download/${COSIGN_VERSION}/${_c_asset}"
  _c_dest="${WORKDIR}/${_c_asset}"
  fetch "$_c_url" "$_c_dest" || die "could not download cosign from ${_c_url}"
  _c_have="$(sha256_of "$_c_dest")"
  [ "$_c_have" = "$_c_sha" ] \
    || die "cosign download failed its SHA-256 check (got ${_c_have})"
  chmod +x "$_c_dest"
  COSIGN="$_c_dest"
}

# ---------------------------------------------------------------------------
# Verify the downloaded release. Both the checksum and the signature must
# pass before this returns; any mismatch aborts.
# ---------------------------------------------------------------------------
verify_release() {
  _bin="$1"; _checksums="$2"; _bundle="$3"

  info "verifying SHA-256 checksum"
  _want="$(awk -v f="$ASSET" '$2==f {print $1}' "$_checksums")"
  [ -n "$_want" ] \
    || die "checksums.txt has no entry for ${ASSET}" \
           "the release may not ship this platform; check the release page"
  _got="$(sha256_of "$_bin")"
  [ "$_got" = "$_want" ] \
    || die "SHA-256 mismatch for ${ASSET}: expected ${_want}, got ${_got}" \
           "do not install; re-download, and report this if it persists"
  ok "checksum verified"

  info "verifying Sigstore signature"
  ensure_cosign
  if [ -n "${KUBENEST_COSIGN_PUBKEY:-}" ]; then
    # Pinned-key mode (air-gapped mirrors, key pinning).
    "$COSIGN" verify-blob "$_checksums" \
      --bundle "$_bundle" \
      --key "$KUBENEST_COSIGN_PUBKEY" >/dev/null \
      || die "signature verification FAILED against the pinned public key" \
             "do not install; the checksums file is not signed by the key you pinned"
  else
    # Keyless mode against the release workflow's GitHub Actions identity.
    "$COSIGN" verify-blob "$_checksums" \
      --bundle "$_bundle" \
      --certificate-identity-regexp "$KEYLESS_IDENTITY_REGEXP" \
      --certificate-oidc-issuer "$KEYLESS_OIDC_ISSUER" >/dev/null \
      || die "signature verification FAILED: checksums.txt is not signed by ${REPO}'s release workflow" \
             "do not install; verify the release tag and try again"
  fi
  ok "signature verified"
}

# ---------------------------------------------------------------------------
# Install the verified binary into $INSTALL_DIR, escalating with sudo only if
# the directory is not writable by the current user.
# ---------------------------------------------------------------------------
install_binary() {
  _bin="$1"
  _target="${INSTALL_DIR}/${BIN_NAME}"

  # Probe whether the destination is usable as-is: it must exist (or be
  # creatable) and be writable. A scratch file is the reliable test.
  _need_sudo=""
  if [ ! -d "$INSTALL_DIR" ] || ! ( : > "${INSTALL_DIR}/.kubenest-install-probe" ) 2>/dev/null; then
    _need_sudo="yes"
  else
    rm -f "${INSTALL_DIR}/.kubenest-install-probe"
  fi

  if [ -n "$_need_sudo" ]; then
    command -v sudo >/dev/null 2>&1 \
      || die "cannot write to ${INSTALL_DIR} and sudo is not available" \
             "install to a writable directory with --install-dir"
    info "installing to ${INSTALL_DIR} (requires sudo)"
    sudo mkdir -p "$INSTALL_DIR"
    sudo install -m 0755 "$_bin" "$_target" \
      || die "sudo install into ${INSTALL_DIR} failed"
  else
    info "installing to ${INSTALL_DIR}"
    install -m 0755 "$_bin" "$_target" \
      || die "install into ${INSTALL_DIR} failed"
  fi

  ok "installed ${_target}"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
  printf '\033[1m kubenest installer\033[0m\n'
  printf ' verified download of the KubeNest platform CLI\n\n'

  detect_platform
  resolve_version

  if already_installed; then
    exit 0
  fi

  # Substitute the resolved version into the asset name.
  ASSET="${BIN_NAME}-${VERSION}-${OS}-${ARCH}"
  _download_url="${RELEASE_BASE}/download/${VERSION}/${ASSET}"
  _checksums_url="${RELEASE_BASE}/download/${VERSION}/checksums.txt"
  _bundle_url="${RELEASE_BASE}/download/${VERSION}/checksums.txt.sigstore.json"

  _bin="${WORKDIR}/${ASSET}"
  _checksums="${WORKDIR}/checksums.txt"
  _bundle="${WORKDIR}/checksums.txt.sigstore.json"

  info "downloading ${ASSET}"
  fetch "$_download_url" "$_bin" \
    || die "could not download ${_download_url}" \
           "check that release ${VERSION} exists and ships ${ASSET}"
  fetch "$_checksums_url" "$_checksums" \
    || die "could not download checksums.txt for ${VERSION}"
  fetch "$_bundle_url" "$_bundle" \
    || die "could not download the Sigstore bundle for ${VERSION}" \
           "releases before ${BIN_NAME} adopted bundle signing cannot be verified by this installer"

  verify_release "$_bin" "$_checksums" "$_bundle"
  install_binary "$_bin"

  # Post-install smoke check: the binary must report its version.
  if "${INSTALL_DIR}/${BIN_NAME}" --version >/dev/null 2>&1; then
    ok "verified: $( "${INSTALL_DIR}/${BIN_NAME}" --version 2>/dev/null || echo installed )"
  else
    warn "installed but the binary did not respond to --version; check it runs"
  fi

  printf '\n'
  ok "kubenest is installed at ${INSTALL_DIR}/${BIN_NAME}"
  info "next: run 'kubenest login', then 'kubenest platform install' — see docs.kubenest.io/install"
}

main "$@"
