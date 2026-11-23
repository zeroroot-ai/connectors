#!/usr/bin/env bash
# Failing fixture of digest-table.sh. A fake inspect command reads fixed JSON.
# The test checks the table of each case, and that a failed inspect fails the
# script.
set -uo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT

# The fake inspect reads $FIXTURES/<server>.digest and $FIXTURES/<server>.image.
# A missing file is a failed inspect.
cat >"$dir/inspect" <<'F'
#!/usr/bin/env bash
server="${1##*connectors-}"; server="${server%%:*}"
case "$3" in
  *Manifest.Digest*) cat "$FIXTURES/$server.digest" ;;
  *) cat "$FIXTURES/$server.image" ;;
esac
F
chmod +x "$dir/inspect"
export FIXTURES="$dir" INSPECT="$dir/inspect"

fail() { echo "FAIL: $*" >&2; exit 1; }

echo '"sha256:aaa"' >"$dir/plain.digest"
echo '{"config":{"Labels":{"org.opencontainers.image.title":"x"}}}' >"$dir/plain.image"
echo '"sha256:bbb"' >"$dir/mock.digest"
echo '{"config":{"Labels":{"ai.zeroroot.image-class":"test"}}}' >"$dir/mock.image"
echo '"sha256:ccc"' >"$dir/multi.digest"
echo '{"linux/amd64":{"config":{"Labels":{"ai.zeroroot.image-class":"test"}}}}' >"$dir/multi.image"

out="$("$here/digest-table.sh" '["plain","mock","multi"]' sha-1234567)" || fail "the script failed on good input"
connectors="${out%%### Test images*}"
tests="${out#*### Test images}"
grep -q 'connectors-plain | sha256:aaa' <<<"$connectors" || fail "an image with no label is not in the connector table"
grep -q 'connectors-mock' <<<"$connectors" && fail "a test image is in the connector table"
grep -q 'connectors-multi' <<<"$connectors" && fail "a test image of a multi-platform manifest is in the connector table"
grep -q 'connectors-mock | sha256:bbb' <<<"$tests" || fail "a test image is not in the test table"
grep -q 'connectors-multi | sha256:ccc' <<<"$tests" || fail "a multi-platform test image is not in the test table"
grep -q 'connectors-plain' <<<"$tests" && fail "a connector is in the test table"

out="$("$here/digest-table.sh" '["plain"]' sha-1234567)" || fail "the script failed with no test image"
grep -q 'Test images' <<<"$out" && fail "the test section shows with no test image"

# A failed inspect must fail the script and print no table.
rm "$dir/mock.image"
if out="$("$here/digest-table.sh" '["plain","mock"]' sha-1234567 2>/dev/null)"; then
  fail "a failed inspect did not fail the script"
fi
[ -z "$out" ] || fail "the script printed a table after a failed inspect"
rm "$dir/plain.digest"
"$here/digest-table.sh" '["plain"]' sha-1234567 >/dev/null 2>&1 && fail "a failed digest read did not fail the script"
echo "ok"
