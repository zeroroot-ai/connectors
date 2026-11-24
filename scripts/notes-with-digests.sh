#!/usr/bin/env bash
# Print release notes with one "## Image digests" section at the end. The
# script removes an older section first, so a rerun does not add a second one.
# The old section ends at the next "## " heading, or at the end of the notes.
# Usage: notes-with-digests.sh <notes file> <table file>
set -euo pipefail
notes="${1:?usage: notes-with-digests.sh <notes file> <table file>}"
table="${2:?usage: notes-with-digests.sh <notes file> <table file>}"
awk '
  /^## Image digests[[:space:]]*$/ { skip = 1; next }
  /^## / { skip = 0 }
  !skip { print }
' "$notes" | sed -e :a -e '/^\n*$/{$d;N;ba' -e '}'
echo
echo "## Image digests"
echo
cat "$table"
