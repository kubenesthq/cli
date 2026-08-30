#!/bin/sh
# test/install/contract_test.sh — assert that install.sh and the release
# workflow describe the SAME release.
#
# WHY THIS EXISTS, and it is the whole reason: install.sh once downloaded
# checksums.txt.sigstore.json and verified it with `cosign verify-blob
# --bundle`, while .github/workflows/build.yml signed with
# --output-signature/--output-certificate and produced no bundle at all. The
# installer could not install any release this repository cut. Every test
# passed, because test/install/smoke.sh built its own fixture release in the
# shape the installer expected — the test and the installer agreed with each
# other and both disagreed with the thing that actually produces releases.
#
# No container, no network, no Docker: this reads both files and compares them,
# so it runs on every CI job rather than on demand. A contract this cheap to
# check should never be checked by hand again.
#
# Run: sh test/install/contract_test.sh

set -eu

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
INSTALL_SH="${ROOT}/install.sh"
BUILD_YML="${ROOT}/.github/workflows/build.yml"
REPO="kubenesthq/cli"

fails=0
ok()   { printf '  ok   %s\n' "$*"; }
bad()  { printf '  FAIL %s\n' "$*" >&2; fails=$((fails + 1)); }

[ -r "$INSTALL_SH" ] || { echo "cannot read $INSTALL_SH" >&2; exit 1; }
[ -r "$BUILD_YML" ]  || { echo "cannot read $BUILD_YML" >&2; exit 1; }

echo "install.sh <-> build.yml release contract"

# COMMENT-ONLY LINES ARE STRIPPED before anything is matched. This file explains
# at length WHY the bundle is the artifact and why the detached flags are gone,
# and prose naming a filename is not a workflow that produces it. Without this,
# a comment mentioning checksums.txt.sigstore.json would satisfy the very check
# that exists to prove the workflow emits it — a verifier passing on its own
# documentation. Everything else is checked: run: blocks and the release-notes
# body alike, deliberately, because a verification example a v3 user cannot run
# is also a defect.
WORKFLOW="$(sed '/^[[:space:]]*#/d' "$BUILD_YML")"
in_workflow() { printf '%s\n' "$WORKFLOW" | grep -q "$@"; }

# --- 1. every release artifact install.sh fetches must be one build.yml
# actually PRODUCES.
#
# Checking that the workflow MENTIONS the filename is not enough, and this test
# was wrong that way on its first draft: the release-notes body names
# checksums.txt.sigstore.json in its verification example, so a workflow that
# documented the bundle while signing into a detached .sig/.pem still passed.
# So each artifact is paired with the command that creates it, and it is that
# command the check looks for.
check_produced() {
  _artifact="$1"; _producer="$2"
  grep -qF -- "$_artifact" "$INSTALL_SH" || { bad "install.sh no longer fetches $_artifact — update this test"; return; }
  if in_workflow -F -- "$_producer"; then
    ok "build.yml produces $_artifact ($_producer)"
  else
    bad "install.sh downloads $_artifact and no step in build.yml creates it (this is the kn-s9ck defect)"
  fi
}
check_produced checksums.txt                 "sha256sum * > checksums.txt"
check_produced checksums.txt.sigstore.json   "--bundle dist/checksums.txt.sigstore.json"

# The per-platform binary: install.sh composes kubenest-<tag>-<os>-<arch>, the
# workflow writes dist/kubenest-${TAG}-${os}-${arch}${ext}. Compare the shape.
#
# The single quotes are the point: these are grep patterns matching the LITERAL
# text "${BIN_NAME}" etc. in the two files, not expansions. Expanding them here
# would compare this script's empty variables and pass on anything.
# shellcheck disable=SC2016
if grep -q 'ASSET="\${BIN_NAME}-\${VERSION}-\${OS}-\${ARCH}"' "$INSTALL_SH" \
   && in_workflow -- 'dist/kubenest-\${TAG}-\${os}-\${arch}\${ext}'; then
  ok "the binary asset name is composed the same way on both sides"
else
  bad "the binary asset name shape differs between install.sh and build.yml"
fi

# --- 2. the signing artifact must be uploaded, not merely created
if in_workflow -E '^[[:space:]]+files: dist/\*[[:space:]]*$'; then
  ok "build.yml uploads all of dist/, so the bundle ships as a release asset"
else
  bad "build.yml's release step does not upload dist/* — an unuploaded bundle is the same 404 as no bundle"
fi

# --- 3. cosign v3 removed sign-blob's detached outputs and verify-blob's
# --signature/--certificate. A signer on v2 and a verifier on v3 cannot agree
# on an artifact, so the versions must be pinned together and the removed flags
# must not come back.
inst_cosign="$(sed -n 's/^COSIGN_VERSION="\(.*\)"$/\1/p' "$INSTALL_SH")"
wf_cosign="$(printf '%s\n' "$WORKFLOW" | sed -n 's/^[[:space:]]*cosign-release:[[:space:]]*\(.*\)$/\1/p')"
if [ -z "$wf_cosign" ]; then
  bad "build.yml does not pin cosign-release; the workflow would sign with whatever the installer action defaults to"
elif [ "$inst_cosign" = "$wf_cosign" ]; then
  ok "cosign is pinned to $inst_cosign on both sides"
else
  bad "cosign pins differ: install.sh $inst_cosign, build.yml $wf_cosign"
fi

for removed in --output-signature --output-certificate; do
  if in_workflow -- "$removed"; then
    bad "build.yml uses $removed, which cosign v3's sign-blob does not have"
  else
    ok "build.yml does not use $removed"
  fi
done

# --- 4. the keyless identity must be the same string in both places.
# build.yml's copy lives in the release notes and carries the Actions
# expression ${{ github.repository }}; expand it before comparing.
inst_id="$(sed -n "s/^KEYLESS_IDENTITY_REGEXP='\(.*\)'$/\1/p" "$INSTALL_SH")"
wf_id="$(printf '%s\n' "$WORKFLOW" | sed -n "s/.*--certificate-identity-regexp '\(.*\)'.*/\1/p" | head -1)"
wf_id_expanded="$(printf '%s' "$wf_id" | sed "s|\${{ github.repository }}|${REPO}|")"
if [ -z "$inst_id" ] || [ -z "$wf_id" ]; then
  bad "could not read the identity regexp from install.sh and/or build.yml"
elif [ "$inst_id" = "$wf_id_expanded" ]; then
  ok "the keyless identity regexp is identical in both places"
else
  bad "the keyless identity regexp differs:
         install.sh: $inst_id
         build.yml:  $wf_id_expanded"
fi

# Identity pinning without an anchor is a substring match in Go's regexp, and
# an unrestricted @.* accepts a signature from any ref rather than a release
# tag. Both are what makes keyless verification mean something.
case "$inst_id" in
  '^'*'$') ok "the identity regexp is anchored at both ends" ;;
  *)       bad "the identity regexp is not anchored: cosign matches it as a substring" ;;
esac
case "$inst_id" in
  *'@refs/tags/'*) ok "the identity regexp accepts release tags only" ;;
  *)              bad "the identity regexp accepts any git ref, not just release tags" ;;
esac

inst_issuer="$(sed -n "s/^KEYLESS_OIDC_ISSUER='\(.*\)'$/\1/p" "$INSTALL_SH")"
if in_workflow -F -- "--certificate-oidc-issuer $inst_issuer"; then
  ok "the OIDC issuer matches on both sides"
else
  bad "the OIDC issuer in install.sh ($inst_issuer) does not appear in build.yml"
fi

echo
if [ "$fails" -gt 0 ]; then
  echo "FAILED: $fails contract mismatch(es) between install.sh and build.yml" >&2
  exit 1
fi
echo "all contract checks passed"
