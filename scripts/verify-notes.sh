#!/usr/bin/env bash
# Check the release notes after a release. Each digest in the notes must equal
# the digest that the version tag of the image resolves to. A notes file with
# no digest row fails, and so does a failed inspect.
# Usage: verify-notes.sh <notes file> <version>
# The version has no leading v, for example 0.8.2.
# INSPECT is the inspect command. The default is "docker buildx imagetools inspect".
set -euo pipefail
notes="${1:?usage: verify-notes.sh <notes file> <version>}"
version="${2:?usage: verify-notes.sh <notes file> <version>}"
inspect="${INSPECT:-docker buildx imagetools inspect}"

rows="$(grep -E '^\| ghcr\.io/[^ |]+ \| sha256:[0-9a-f]+ \|$' "$notes" || true)"
if [ -z "$rows" ]; then
  echo "verify-notes: $notes has no image digest row" >&2
  exit 1
fi

status=0
while IFS='|' read -r _ image digest _; do
  image="${image// /}"
  digest="${digest// /}"
  # shellcheck disable=SC2086 # INSPECT holds a command and its words
  actual="$($inspect "$image:$version" --format '{{json .Manifest.Digest}}' | tr -d '"')" || actual=""
  if [ "$actual" != "$digest" ]; then
    echo "verify-notes: $image notes digest $digest, tag $version digest ${actual:-none}" >&2
    status=1
  fi
done <<<"$rows"
exit "$status"
