#!/usr/bin/env bash
# Failing fixture of verify-notes.sh. A fake inspect command reads fixed digests.
set -uo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT

# The fake inspect reads $FIXTURES/<server>.digest. A missing file is a failed inspect.
# It reads stdin to the end, so a call that shares stdin with a loop breaks the loop.
cat >"$dir/inspect" <<'F'
#!/usr/bin/env bash
cat >/dev/null
server="${1##*connectors-}"; server="${server%%:*}"
cat "$FIXTURES/$server.digest"
F
chmod +x "$dir/inspect"
export FIXTURES="$dir" INSPECT="$dir/inspect"

fail() { echo "FAIL: $*" >&2; exit 1; }
run() { "$here/verify-notes.sh" "$1" 0.1.0 "${2:-$servers}" 2>/dev/null; }
servers='["a","b","mock"]'

# Two tables: the connectors table and the Test images table.
cat >"$dir/notes.md" <<'N'
## Image digests

| image | digest |
|---|---|
| ghcr.io/zeroroot-ai/connectors-a | sha256:aaa |
| ghcr.io/zeroroot-ai/connectors-b | sha256:bbb |

### Test images

| image | digest |
|---|---|
| ghcr.io/zeroroot-ai/connectors-mock | sha256:ccc |
N
echo '"sha256:aaa"' >"$dir/a.digest"
echo '"sha256:bbb"' >"$dir/b.digest"
echo '"sha256:ccc"' >"$dir/mock.digest"

run "$dir/notes.md" || fail "matching digests in two tables failed"

# A tag that points to another build must fail.
echo '"sha256:zzz"' >"$dir/b.digest"
run "$dir/notes.md" && fail "a digest mismatch passed"
echo '"sha256:bbb"' >"$dir/b.digest"

# A missing row must fail, also when the other rows match.
grep -v connectors-b "$dir/notes.md" >"$dir/missing.md"
run "$dir/missing.md" && fail "a missing row passed"

# A second row of one image must fail.
{ cat "$dir/notes.md"; echo '| ghcr.io/zeroroot-ai/connectors-a | sha256:aaa |'; } >"$dir/twice.md"
run "$dir/twice.md" && fail "a double row passed"

# A row with a trailing space or a CR must fail.
sed 's/connectors-a | sha256:aaa |/&  /' "$dir/notes.md" >"$dir/space.md"
run "$dir/space.md" && fail "a row with a trailing space passed"
sed 's/connectors-a | sha256:aaa |/&\r/' "$dir/notes.md" >"$dir/cr.md"
run "$dir/cr.md" && fail "a row with a CR passed"

# A row of an image outside the list must fail.
{ cat "$dir/notes.md"; echo '| ghcr.io/zeroroot-ai/connectors-extra | sha256:eee |'; } >"$dir/extra.md"
run "$dir/extra.md" && fail "a row of an unknown image passed"

# A failed inspect must fail.
rm "$dir/b.digest"
run "$dir/notes.md" && fail "a failed inspect passed"
echo '"sha256:bbb"' >"$dir/b.digest"

# Notes with no digest row, and a bad server list, must fail.
echo "no table" >"$dir/empty.md"
run "$dir/empty.md" && fail "notes with no row passed"
run "$dir/notes.md" '[]' && fail "an empty server list passed"
run "$dir/notes.md" 'not json' && fail "a bad server list passed"
echo "ok"
