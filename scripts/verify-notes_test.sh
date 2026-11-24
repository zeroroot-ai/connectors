#!/usr/bin/env bash
# Failing fixture of verify-notes.sh. A fake inspect command reads fixed digests.
set -uo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT

# The fake inspect reads $FIXTURES/<server>.digest. A missing file is a failed inspect.
cat >"$dir/inspect" <<'F'
#!/usr/bin/env bash
server="${1##*connectors-}"; server="${server%%:*}"
cat "$FIXTURES/$server.digest"
F
chmod +x "$dir/inspect"
export FIXTURES="$dir" INSPECT="$dir/inspect"

fail() { echo "FAIL: $*" >&2; exit 1; }

cat >"$dir/notes.md" <<'N'
## Image digests

| image | digest |
|---|---|
| ghcr.io/zeroroot-ai/connectors-a | sha256:aaa |
| ghcr.io/zeroroot-ai/connectors-b | sha256:bbb |
N
echo '"sha256:aaa"' >"$dir/a.digest"
echo '"sha256:bbb"' >"$dir/b.digest"

"$here/verify-notes.sh" "$dir/notes.md" 0.1.0 2>/dev/null || fail "matching digests failed"

# A tag that points to another build must fail.
echo '"sha256:zzz"' >"$dir/b.digest"
"$here/verify-notes.sh" "$dir/notes.md" 0.1.0 2>/dev/null && fail "a digest mismatch passed"
echo '"sha256:bbb"' >"$dir/b.digest"

# A failed inspect must fail.
rm "$dir/b.digest"
"$here/verify-notes.sh" "$dir/notes.md" 0.1.0 2>/dev/null && fail "a failed inspect passed"

# Notes with no digest row must fail.
echo "no table" >"$dir/empty.md"
"$here/verify-notes.sh" "$dir/empty.md" 0.1.0 2>/dev/null && fail "notes with no row passed"
echo "ok"
