#!/usr/bin/env bash
# Failing fixture of check-servers.sh: the fixture tree must fail the guard,
# and the real tree must pass it.
set -uo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
if "$here/check-servers.sh" "$here/testdata/bad" 2>/dev/null; then
  echo "FAIL: the guard accepted a server with no Dockerfile and no README" >&2
  exit 1
fi
"$here/check-servers.sh" "$here/.." || { echo "FAIL: the guard rejects the real tree" >&2; exit 1; }
echo "ok"
