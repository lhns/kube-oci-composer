#!/usr/bin/env bash
# Check that every platform image of a pushed image carries a binary built for that platform.
# The index and the image config claim the right architecture even when the binary does not, so
# this reads the ELF e_machine of the binary itself. No QEMU: a mislabelled amd64 binary would run
# natively on an amd64 runner and pass.
#
# Usage: check-binary-arch.sh <image>@sha256:<digest> <binary path in image> <platform>[,<platform>...]
#   e.g. check-binary-arch.sh ghcr.io/lhns/kube-vnet@sha256:abc... /manager linux/amd64,linux/arm64
# The image's real platforms must be exactly the expected set; attestation manifests are skipped.
# Exit 1: a check failed. Exit 2: usage or tooling error (nothing was proven either way).
# Needs crane, jq, od, tar.
set -euo pipefail

usage() { echo "usage: $0 <image>@sha256:<digest> <binary path> <platform>[,<platform>...]" >&2; exit 2; }
[ $# -eq 3 ] || usage
ref=$1 bin=${2#/} expected=$3
[[ "$ref" == *@sha256:* ]] || { echo "ERROR: $ref is not a digest reference" >&2; exit 2; }
[ -n "$bin" ] && [ -n "$expected" ] || usage
for tool in crane jq od tar; do
  command -v "$tool" > /dev/null || { echo "ERROR: $tool not found" >&2; exit 2; }
done
repo=${ref%@*}

want_machine() { # <platform> -> e_machine bytes as od prints them, or nothing if unsupported
  case "$1" in
    linux/amd64) echo "3e 00" ;;          # EM_X86_64
    linux/arm64|linux/arm64/v8) echo "b7 00" ;;  # EM_AARCH64
  esac
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Real platform manifests as "<platform> <digest>" lines.
manifest=$(crane manifest "$ref") || { echo "ERROR: cannot read manifest of $ref" >&2; exit 2; }
case "$(jq -r '.mediaType // empty' <<< "$manifest")" in
  application/vnd.oci.image.index.v1+json|application/vnd.docker.distribution.manifest.list.v2+json)
    jq -r '.manifests[]
      | select(.annotations["vnd.docker.reference.type"] != "attestation-manifest")
      | select(.platform.os != "unknown")
      | "\(.platform.os)/\(.platform.architecture)\(if .platform.variant then "/" + .platform.variant else "" end) \(.digest)"' \
      <<< "$manifest" > "$tmp/platforms"
    ;;
  *) # A single-platform image: its config says which platform it claims.
    crane config "$ref" | jq -r --arg d "${ref#*@}" \
      '"\(.os)/\(.architecture)\(if .variant then "/" + .variant else "" end) \($d)"' > "$tmp/platforms"
    ;;
esac

fail=0
seen=" "
while read -r platform digest; do
  norm=${platform%/v8}  # buildx may label arm64 as linux/arm64/v8
  if [[ ",$expected," != *",$norm,"* ]]; then
    echo "FAIL $platform: not an expected platform ($expected)"; fail=1; continue
  fi
  if [[ "$seen" == *" $norm "* ]]; then
    echo "FAIL $platform: listed twice"; fail=1; continue
  fi
  seen+="$norm "
  want=$(want_machine "$norm")
  [ -n "$want" ] || { echo "ERROR: no e_machine known for $norm" >&2; exit 2; }
  # By the platform manifest's digest, so crane does no platform matching of its own.
  out="$tmp/${norm//\//_}"
  crane export "$repo@$digest" "$tmp/fs.tar" || { echo "ERROR: cannot export $repo@$digest" >&2; exit 2; }
  tar -xOf "$tmp/fs.tar" "$bin" > "$out" 2> /dev/null || true
  rm -f "$tmp/fs.tar"
  if [ ! -s "$out" ]; then
    echo "FAIL $platform: /$bin missing, empty or not a regular file (a symlink?) in $repo@$digest"; fail=1; continue
  fi
  magic=$(od -An -tx1 -N4 "$out" | xargs)
  got=$(od -An -tx1 -j18 -N2 "$out" | xargs)
  if [ "$magic" != "7f 45 4c 46" ]; then
    echo "FAIL $platform: /$bin is not an ELF file (starts with '$magic')"; fail=1
  elif [ "$got" != "$want" ]; then
    echo "FAIL $platform: /$bin has e_machine '$got', expected '$want'"; fail=1
  else
    echo "ok   $platform: /$bin e_machine $got ($digest)"
  fi
done < "$tmp/platforms"

IFS=, read -ra platforms <<< "$expected"
for p in "${platforms[@]}"; do
  [ -n "$(want_machine "$p")" ] || { echo "ERROR: unsupported expected platform $p" >&2; exit 2; }
  [[ "$seen" == *" $p "* ]] || { echo "FAIL $p: no such platform image in $ref"; fail=1; }
done
exit "$fail"
