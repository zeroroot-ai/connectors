#!/usr/bin/env bash
# Check the release notes after a release. Each server must have exactly one
# digest row in the notes, and the digest must equal the digest that the
# version tag of the image resolves to. A missing row, a second row, a row
# with a trailing space or a CR, a row of an unknown image, and a failed
# inspect all fail the script.
# Usage: verify-notes.sh <notes file> <version> <servers JSON array>
# The version has no leading v, for example 0.8.2. The servers list is the
# list that digest-table.sh reads.
# INSPECT is the inspect command. The default is "docker buildx imagetools inspect".
set -euo pipefail
usage="usage: verify-notes.sh <notes file> <version> <servers JSON array>"
notes="${1:?$usage}"
version="${2:?$usage}"
servers="${3:?$usage}"
inspect="${INSPECT:-docker buildx imagetools inspect}"

names="$(jq -er '.[]' <<<"$servers")"
if [ -z "$names" ]; then
  echo "verify-notes: the list of servers is empty" >&2
  exit 1
fi

status=0
count=0
for s in $names; do
  count=$((count + 1))
  image="ghcr.io/zeroroot-ai/connectors-$s"
  # A row starts with the image name. The exact form is checked after the count.
  found="$(grep -cF "| $image |" "$notes" || true)"
  if [ "$found" != 1 ]; then
    echo "verify-notes: $image has $found rows in the notes, need exactly 1" >&2
    status=1
    continue
  fi
  row="$(grep -F "| $image |" "$notes")"
  if ! [[ "$row" =~ ^\|\ [^\ \|]+\ \|\ (sha256:[0-9a-f]+)\ \|$ ]]; then
    echo "verify-notes: the row of $image is not well formed" >&2
    status=1
    continue
  fi
  digest="${BASH_REMATCH[1]}"
  # shellcheck disable=SC2086 # INSPECT holds a command and its words
  actual="$($inspect "$image:$version" --format '{{json .Manifest.Digest}}' </dev/null | tr -d '"')" || actual=""
  if [ "$actual" != "$digest" ]; then
    echo "verify-notes: $image notes digest $digest, tag $version digest ${actual:-none}" >&2
    status=1
  fi
done

# A row of an image outside the list is also a defect.
rows="$(grep -c '^| ghcr\.io/' "$notes" || true)"
if [ "$rows" != "$count" ]; then
  echo "verify-notes: the notes have $rows image rows, the list has $count servers" >&2
  status=1
fi
exit "$status"
