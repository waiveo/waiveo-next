#!/bin/sh
# scripts/install-from-channel-index.sh -- channel-index/1 install/update client
# for a relay's own `relay-release` build (contracts/channel-index.md).
#
# Given a channel's already-fetched-address (INDEX_URL) and the channel
# identifier it is expected to name (CHANNEL, e.g. relay-v1), this script:
#   1. fetches the ChannelIndex document (Wire shapes) at INDEX_URL;
#   2. resolves every `kind: relay-release`, `arch: linux/amd64`,
#      `status: active` artifact entry (Channel namespaces CHI-041/043,
#      Index schema CHI-020) -- exactly the entries a relay bound to this
#      channel is ever allowed to install;
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
# digest/size verification) against the one channel document INDEX_URL
# already names. It does NOT perform the root/targets/snapshot/timestamp
# signature chain (CHI-001-012, CHI-050 steps 1-6) or the revocation-feed
# check (CHI-070-074) -- establishing INDEX_URL as a trust-established
# channel source, and checking it hasn't been revoked, is the deployed
# unit's channel-binding concern (Scope, CHI-042), upstream of this script.
# It also does not implement split artifacts (`parts`, CHI-024) or
# transport compression (`compression`, CHI-026): an entry carrying either
# is refused rather than mishandled (fail closed on an unsupported shape).
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

# Resolve every currently-active relay-release entry for ARCH (CHI-020,
# CHI-041/043) into a tab-separated line per entry. jq's own filter is the
# ONE place kind/arch/status selection happens, so the shell loop below
# never re-implements that policy.
matches_file="$stage_dir/matches.tsv"
if ! jq -r --arg arch "$ARCH" '
    (.signed.artifacts // [])[]
    | select(.kind == "relay-release" and .arch == $arch and .status == "active")
    | [ .artifact_id, .version, .digest, (.size | tostring), .download_url,
        (has("parts") | tostring), (has("compression") | tostring) ]
    | @tsv
  ' "$index_file" >"$matches_file"; then
  echo "install-from-channel-index: failed to parse .signed.artifacts from $INDEX_URL" >&2
  exit 1
fi

if [ ! -s "$matches_file" ]; then
  echo "install-from-channel-index: no active relay-release artifact for arch '$ARCH' in channel '$CHANNEL'" >&2
  exit 1
fi

# Reject an index carrying more than one active entry for the same
# artifact_id/arch -- ambiguous, and this script has no basis to prefer one
# over the other (that's exactly the "current version" this script is
# supposed to resolve unambiguously).
dupe_ids=$(cut -f1 "$matches_file" | sort | uniq -d)
if [ -n "$dupe_ids" ]; then
  echo "install-from-channel-index: ambiguous index -- more than one active relay-release/$ARCH entry for artifact_id(s): $dupe_ids" >&2
  exit 1
fi

# Pass 1: download + verify every resolved entry into stage_dir, all-or-
# nothing. Nothing under INSTALL_ROOT is touched until every entry this run
# resolved has verified clean, so a mismatch on entry N never leaves a
# partially-updated install from entries 1..N-1.
verified_ids=""
while IFS="$(printf '\t')" read -r artifact_id version digest size download_url has_parts has_compression; do
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
  echo "install-from-channel-index: verified $artifact_id $version (sha256:$actual_hex, $actual_size bytes)"
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
