#!/usr/bin/env bash
# Failing fixture of notes-with-digests.sh. A rerun must keep one section.
set -uo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

printf '| image | digest |\n|---|---|\n| ghcr.io/zeroroot-ai/connectors-a | sha256:new |\n' >"$dir/table.md"
printf '## 0.1.0\n\n* a change\n' >"$dir/base.md"

"$here/notes-with-digests.sh" "$dir/base.md" "$dir/table.md" >"$dir/once.md" || fail "the first run failed"
[ "$(grep -c '^## Image digests' "$dir/once.md")" = 1 ] || fail "the first run has no single section"
grep -q 'a change' "$dir/once.md" || fail "the first run lost the notes"

# The rerun reads notes that hold an old section, followed by another section.
sed 's/sha256:new/sha256:old/' "$dir/once.md" >"$dir/old.md"
printf '\n## After\n\ntext\n' >>"$dir/old.md"
"$here/notes-with-digests.sh" "$dir/old.md" "$dir/table.md" >"$dir/twice.md" || fail "the rerun failed"
[ "$(grep -c '^## Image digests' "$dir/twice.md")" = 1 ] || fail "a rerun left two sections"
grep -q 'sha256:old' "$dir/twice.md" && fail "a rerun kept the old digest"
grep -q 'sha256:new' "$dir/twice.md" || fail "a rerun lost the new digest"
grep -q 'a change' "$dir/twice.md" || fail "a rerun lost the notes"
grep -q '^## After' "$dir/twice.md" || fail "a rerun removed a later section"

# Running on its own output must not change it.
"$here/notes-with-digests.sh" "$dir/twice.md" "$dir/table.md" >"$dir/thrice.md" || fail "the third run failed"
[ "$(grep -c '^## Image digests' "$dir/thrice.md")" = 1 ] || fail "the third run has two sections"
echo "ok"
