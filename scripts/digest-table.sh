#!/usr/bin/env bash
# Print the Markdown digest table of the images of a release. An image with the
# label ai.zeroroot.image-class=test goes in its own "Test images" table, so a
# release never lists a test image as a connector. A failed inspect fails the
# script, and it prints no table.
# Usage: digest-table.sh <servers JSON array> <tag>
# INSPECT is the inspect command. The default is "docker buildx imagetools inspect".
set -euo pipefail
servers="${1:?usage: digest-table.sh <servers JSON array> <tag>}"
tag="${2:?usage: digest-table.sh <servers JSON array> <tag>}"
inspect="${INSPECT:-docker buildx imagetools inspect}"

# Read the names first. A failed jq in a for list does not stop the script.
names="$(jq -er '.[]' <<<"$servers")"
if [ -z "$names" ]; then
  echo "digest-table: the list of servers is empty" >&2
  exit 1
fi

connector_rows=""
test_rows=""
for s in $names; do
  ref="ghcr.io/zeroroot-ai/connectors-$s:$tag"
  # shellcheck disable=SC2086 # INSPECT holds a command and its words
  digest="$($inspect "$ref" --format '{{json .Manifest.Digest}}' | tr -d '"')"
  # shellcheck disable=SC2086
  class="$($inspect "$ref" --format '{{json .Image}}' |
    jq -r '[.. | objects | .["ai.zeroroot.image-class"]? | strings] | first // ""')"
  if [ -z "$digest" ]; then
    echo "digest-table: $ref has no digest" >&2
    exit 1
  fi
  row="| ghcr.io/zeroroot-ai/connectors-$s | $digest |"
  if [ "$class" = test ]; then
    test_rows+="$row"$'\n'
  else
    connector_rows+="$row"$'\n'
  fi
done

echo "| image | digest |"
echo "|---|---|"
printf '%s' "$connector_rows"
if [ -n "$test_rows" ]; then
  echo
  echo "### Test images"
  echo
  echo "These images are for tests. They are no connectors. Do not pin them in a catalog manifest."
  echo
  echo "| image | digest |"
  echo "|---|---|"
  printf '%s' "$test_rows"
fi
