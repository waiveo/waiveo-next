#!/bin/sh
# scripts/install-from-channel-index.sh -- channel-index/1 install/update client
# for a relay's own `relay-release` build (contracts/channel-index.md).
#
# Given a channel's already-fetched-address (INDEX_URL) and the channel
# identifier it is expected to name (CHANNEL, e.g. relay-v1), this script:
#   1. fetches the ChannelIndex document (Wire shapes) at INDEX_URL;
#   2. resolves every `kind: relay-release`, `arch: linux/amd64`,
#      `status: active` artifact entry that is ALSO resolution-time eligible
#      per its own `hold_hours`/`published_at`/`security_flagged` (Channel
#      namespaces CHI-041/043, Index schema CHI-020, staged-rollout hold
#      CHI-029/030) -- exactly the entries a relay bound to this channel is
#      allowed to install right now;
#   3. downloads each resolved entry's `download_url` and verifies the
#      downloaded bytes against that entry's own signed-in-band `digest`
#      (CHI-021) and `size` (CHI-023), hard-failing the whole run on any
#      mismatch before installing anything;
#   4. installs every verified binary under INSTALL_ROOT, backing up
#      whatever it replaces to "<name>.prev" (rollback: `cp
#      INSTALL_ROOT/<name>.prev INSTALL_ROOT/<name>`);
#   5. restarts SYSTEMD_UNITS, if given.
#
# Scope note: this script performs ONLY CHI-050 steps 7-8 (download, then
# digest/size verification) against whatever document INDEX_URL happens to
# serve. It does NOT perform the root/targets/snapshot/timestamp signature
# chain (CHI-001-012, CHI-050 steps 1-6) or the revocation-feed check
# (CHI-070-074, CHI-050 step 9). That gap is NOT this contract's CHI-042
# pushing the concern "upstream" to the deployed unit, despite what an
# earlier version of this comment claimed: CHI-042 governs only which
# channel IDENTIFIER a unit's update source binds to (deployment
# configuration), and the contract's own Scope section places verification
# (steps 1-6 and 9) squarely IN this contract -- it ends only at "this
# artifact is verified and its entry is not revoked," never at "install
# it." The real reason this script stops at steps 7-8 is that the
# root/targets/snapshot/timestamp trust bundle and the revocation feed those
# steps depend on are not implemented anywhere in this repository yet
# (conformance/traceability/channel-index.md marks CHI-001/002/006/008/012/
# 040-043/053/063/064/070-074/080 as TBD-wave1) -- there is nothing yet for
# this script to verify against.
#
# Concretely: the digest/size check this script performs (CHI-021/023) only
# proves the downloaded bytes are internally self-consistent with whatever
# index document INDEX_URL served -- it does NOT prove that document is
# authentic. A party able to influence what INDEX_URL serves (DNS hijack,
# on-path MITM, a compromised host/CDN) can make this script "verify" and
# install artifact bytes of their own choosing. This script provides NO
# cryptographic trust in INDEX_URL's contents; a caller MUST already trust
# INDEX_URL by some means outside this script (a restricted network path, a
# pinned known-good host, etc.) until CHI-050 steps 1-6 and 9 are
# implemented. Accordingly, the log line this script prints on a clean
# match says "digest/size-verified", never bare "verified", so it cannot be
# read as more assurance than it is.
#
# It also does not implement split artifacts (`parts`, CHI-024) or
# transport compression (`compression`, CHI-026): an entry carrying either
# is refused rather than mishandled (fail closed on an unsupported shape).
#
# Known schema-matching limitation: CHI-020's minimum field list for an
# artifact entry does not include `arch`, but this script's ARCH match
# (below) requires an exact `.arch == $ARCH` -- an otherwise CHI-020-
# conformant `relay-release` entry that omits `arch` is excluded rather than
# resolved. The contract defines no matching semantics for an arch-omitting
# entry (no stated "matches any arch" or wildcard rule), so this script
# fails closed (excludes it, same as a genuine arch mismatch) rather than
# guess at unspecified behavior.
#
# Usage:
#   install-from-channel-index.sh INDEX_URL CHANNEL
#
# Args:
#   INDEX_URL  The channel's own stable index URL (CHI-080), fetched as-is.
#              Expected to be a ChannelIndex signed envelope (Wire shapes)
#              whose .signed.channel MUST equal CHANNEL -- a sanity check
#              only; this script verifies no signature over "signed".
#   CHANNEL    The channel identifier this index is expected to name, e.g.
#              relay-v1 (Channel namespaces CHI-040) -- either channel
#              family is valid: a relay channel, or a platform-train
#              channel carrying `relay-release` entries alongside its own
#              (CHI-041/043).
#
# Env:
#   INSTALL_ROOT   Install directory. Default: /opt/waiveo-next/bin
#   SYSTEMD_UNITS  Space-separated systemd unit names `systemctl restart`ed
#                  after a successful install. Default: unset (no restart).
#   ARCH           Artifact arch to resolve. Default: linux/amd64
#   NOW_MS         Resolution-time clock, Unix epoch milliseconds, used only
#                  to evaluate `hold_hours`/`published_at` eligibility
#                  (CHI-029/030). Default: the real current time. Override
#                  for a reproducible run (e.g. under test).
#
# Requires: sh (POSIX), curl, jq, and either sha256sum or `shasum -a 256`.
# systemctl only if SYSTEMD_UNITS is set.
#
# Channel identity (see cmd/waiveo-relay's buildVersion/buildChannel):
# a released relay binary is built with its channel-index/1 identity baked
# in via -ldflags, e.g. for the "1.4.2" entry on channel "relay-v1":
#
#   go build -ldflags "-X main.buildVersion=1.4.2 -X main.buildChannel=relay-v1" \
#     -o waiveo-relay ./cmd/waiveo-relay
#
# `./waiveo-relay --version` then prints that identity back
# (cmd/waiveo-relay's printVersion), so what a box reports running always
# names the exact channel-index/1 entry it was installed from.

set -eu

usage() {
  echo "usage: $0 INDEX_URL CHANNEL" >&2
}

if [ "$#" -ne 2 ]; then
  usage
  exit 2
fi

INDEX_URL=$1
CHANNEL=$2
INSTALL_ROOT=${INSTALL_ROOT:-/opt/waiveo-next/bin}
SYSTEMD_UNITS=${SYSTEMD_UNITS:-}
ARCH=${ARCH:-linux/amd64}
NOW_MS=${NOW_MS:-$(($(date -u +%s) * 1000))}

echo "install-from-channel-index: WARNING -- this run performs only CHI-050 steps 7-8 (artifact digest/size check against $INDEX_URL's own content); it does NOT verify $INDEX_URL's signature chain (CHI-001-012) or check the revocation feed (CHI-070-074). Only point INDEX_URL at a source already trusted by some means outside this script." >&2

for dep in curl jq; do
  if ! command -v "$dep" >/dev/null 2>&1; then
    echo "install-from-channel-index: required dependency '$dep' not found in PATH" >&2
    exit 1
  fi
done

if command -v sha256sum >/dev/null 2>&1; then
  sha256_tool=sha256sum
elif command -v shasum >/dev/null 2>&1; then
  sha256_tool=shasum
else
  echo "install-from-channel-index: neither sha256sum nor shasum found in PATH" >&2
  exit 1
fi

# sha256_hex FILE -- prints FILE's sha256 digest as lowercase hex, no
# filename suffix, regardless of which of the two tools above is present.
sha256_hex() {
  if [ "$sha256_tool" = "sha256sum" ]; then
    sha256sum "$1" | awk '{ print $1 }'
  else
    shasum -a 256 "$1" | awk '{ print $1 }'
  fi
}

# byte_size FILE -- prints FILE's exact byte count with no surrounding
# whitespace (wc -c pads its output on some platforms).
byte_size() {
  wc -c <"$1" | tr -d '[:space:]'
}

# field_sep is the ASCII Unit Separator (0x1F), used below as the delimiter
# for the resolved-artifact records this script parses. Tab (jq's @tsv) is
# NOT used: tab is one of POSIX's "IFS white space" characters, so `read`
# and shell word-splitting collapse consecutive tabs and strip leading/
# trailing ones instead of treating each as an independent field boundary --
# silently misaligning every field after the first empty one on any record
# with an empty field. 0x1F is not IFS white space, so each occurrence
# delimits exactly one field, empty fields included, with nothing collapsed
# or stripped.
field_sep=$(printf '\037')

stage_dir=$(mktemp -d)
cleanup() {
  rm -rf "$stage_dir"
}
trap cleanup EXIT INT TERM

index_file="$stage_dir/index.json"
if ! curl -fsSL "$INDEX_URL" -o "$index_file"; then
  echo "install-from-channel-index: failed to download index from $INDEX_URL" >&2
  exit 1
fi

if ! jq -e . "$index_file" >/dev/null 2>&1; then
  echo "install-from-channel-index: $INDEX_URL did not return valid JSON" >&2
  exit 1
fi

format_version=$(jq -r '.signed.format_version // empty' "$index_file")
if [ -z "$format_version" ]; then
  echo "install-from-channel-index: index has no .signed.format_version (CHI-090)" >&2
  exit 1
fi
format_major=${format_version%%.*}
if [ "$format_major" != "1" ]; then
  echo "install-from-channel-index: unsupported format_version '$format_version' (only major 1 is understood, CHI-090)" >&2
  exit 1
fi

doc_channel=$(jq -r '.signed.channel // empty' "$index_file")
if [ "$doc_channel" != "$CHANNEL" ]; then
  echo "install-from-channel-index: index at $INDEX_URL names channel '$doc_channel', expected '$CHANNEL'" >&2
  exit 1
fi

# Resolve every currently-active, resolution-time-eligible relay-release
# entry for ARCH (CHI-020, CHI-041/043) into one field_sep-delimited record
# per entry. jq's own filter is the ONE place kind/arch/status/hold_hours
# selection happens, so the shell loop below never re-implements that
# policy. The second `select` is CHI-029/030's staged-rollout gate: an
# entry carrying a nonzero `hold_hours` is ineligible for a new install/
# update decision until `hold_hours` hours have elapsed since its own
# `published_at`, UNLESS the entry is marked `security_flagged: true`
# (CHI-030's stated exception for a release superseding a disclosed
# vulnerability); an entry carrying no `hold_hours` is immediately eligible.
matches_file="$stage_dir/matches.tsv"
if ! jq -r --arg arch "$ARCH" --argjson now_ms "$NOW_MS" --arg sep "$field_sep" '
    (.signed.artifacts // [])[]
    | select(.kind == "relay-release" and .arch == $arch and .status == "active")
    | select(
        (.hold_hours // 0) == 0
        or .security_flagged == true
        or ((.published_at // 0) + ((.hold_hours // 0) * 3600000)) <= $now_ms
      )
    | [ .artifact_id, .version, .digest, (.size | tostring), .download_url,
        (has("parts") | tostring), (has("compression") | tostring) ]
    | join($sep)
  ' "$index_file" >"$matches_file"; then
  echo "install-from-channel-index: failed to parse .signed.artifacts from $INDEX_URL" >&2
  exit 1
fi

if [ ! -s "$matches_file" ]; then
  echo "install-from-channel-index: no active relay-release artifact for arch '$ARCH' in channel '$CHANNEL' (after CHI-029/030 hold_hours eligibility filtering)" >&2
  exit 1
fi

# Reject an index carrying more than one active, eligible entry for the same
# artifact_id/arch -- ambiguous, and this script has no basis to prefer one
# over the other (that's exactly the "current version" this script is
# supposed to resolve unambiguously).
dupe_ids=$(cut -d "$field_sep" -f1 "$matches_file" | sort | uniq -d)
if [ -n "$dupe_ids" ]; then
  echo "install-from-channel-index: ambiguous index -- more than one active relay-release/$ARCH entry for artifact_id(s): $dupe_ids" >&2
  exit 1
fi

# Pass 1: download + verify every resolved entry into stage_dir, all-or-
# nothing. Nothing under INSTALL_ROOT is touched until every entry this run
# resolved has verified clean, so a mismatch on entry N never leaves a
# partially-updated install from entries 1..N-1.
verified_ids=""
while IFS="$field_sep" read -r artifact_id version digest size download_url has_parts has_compression; do
  # artifact_id becomes a filesystem path component both in the stage dir
  # (below) and under INSTALL_ROOT (Pass 2) -- and, unlike this index
  # document's other fields, it is never digest/size-checked against
  # anything, because it names the very path that check is performed at.
  # Restrict it to a plain, single-component filename before it is used to
  # build any path: reject empty, ".", "..", or anything containing a "/"
  # (no path traversal component survives an allowlist this strict) or any
  # character outside [A-Za-z0-9._-].
  case "$artifact_id" in
    '' | . | ..)
      echo "install-from-channel-index: invalid artifact_id '$artifact_id' -- must be a non-empty plain filename, not '.' or '..'" >&2
      exit 1
      ;;
  esac
  case "$artifact_id" in
    *[!A-Za-z0-9._-]*)
      echo "install-from-channel-index: invalid artifact_id '$artifact_id' -- only letters, digits, '.', '_', '-' are allowed (no path separators)" >&2
      exit 1
      ;;
  esac

  if [ "$has_parts" = "true" ]; then
    echo "install-from-channel-index: $artifact_id $version carries split 'parts' (CHI-024); split artifacts are not supported by this script" >&2
    exit 1
  fi
  if [ "$has_compression" = "true" ]; then
    echo "install-from-channel-index: $artifact_id $version carries 'compression' (CHI-026); compressed artifacts are not supported by this script" >&2
    exit 1
  fi
  case "$digest" in
    sha256:*) expected_hex=${digest#sha256:} ;;
    *)
      echo "install-from-channel-index: $artifact_id $version has unsupported digest scheme '$digest' (only sha256: is supported)" >&2
      exit 1
      ;;
  esac

  artifact_file="$stage_dir/$artifact_id"
  if ! curl -fsSL "$download_url" -o "$artifact_file"; then
    echo "install-from-channel-index: failed to download $artifact_id $version from $download_url" >&2
    exit 1
  fi

  actual_hex=$(sha256_hex "$artifact_file")
  if [ "$actual_hex" != "$expected_hex" ]; then
    echo "install-from-channel-index: DIGEST_MISMATCH: $artifact_id $version -- expected sha256:$expected_hex, got sha256:$actual_hex" >&2
    exit 1
  fi

  actual_size=$(byte_size "$artifact_file")
  if [ "$actual_size" != "$size" ]; then
    echo "install-from-channel-index: SIZE_MISMATCH: $artifact_id $version -- expected $size bytes, got $actual_size" >&2
    exit 1
  fi

  verified_ids="$verified_ids $artifact_id"
  echo "install-from-channel-index: digest/size-verified $artifact_id $version (sha256:$actual_hex, $actual_size bytes)"
done <"$matches_file"

# Pass 2: install. Every entry in verified_ids already passed digest/size
# verification above, so this pass is pure filesystem work.
mkdir -p "$INSTALL_ROOT"
for artifact_id in $verified_ids; do
  dest="$INSTALL_ROOT/$artifact_id"
  dest_new="$INSTALL_ROOT/.$artifact_id.new"
  cp "$stage_dir/$artifact_id" "$dest_new"
  chmod 0755 "$dest_new"
  if [ -e "$dest" ]; then
    cp -p "$dest" "$dest.prev"
  fi
  mv -f "$dest_new" "$dest"
  echo "install-from-channel-index: installed $dest"
done

if [ -n "$SYSTEMD_UNITS" ]; then
  if ! command -v systemctl >/dev/null 2>&1; then
    echo "install-from-channel-index: SYSTEMD_UNITS set but systemctl not found in PATH" >&2
    exit 1
  fi
  for unit in $SYSTEMD_UNITS; do
    echo "install-from-channel-index: restarting $unit"
    systemctl restart "$unit"
  done
fi

echo "install-from-channel-index: done"
