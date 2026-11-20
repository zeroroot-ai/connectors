#!/usr/bin/env bash
# Fail when a server directory (a directory with a main.go) lacks a Dockerfile
# or a README.md, or when a Dockerfile does not run as non-root. The image
# workflow builds each directory that has a Dockerfile, so a server without one
# would never ship. Usage: check-servers.sh <root>
set -euo pipefail
root="${1:-.}"
fail=0
for main in "$root"/*/main.go; do
  [ -e "$main" ] || continue
  dir="$(dirname "$main")"
  for need in Dockerfile README.md; do
    if [ ! -f "$dir/$need" ]; then echo "check-servers: $dir has no $need" >&2; fail=1; fi
  done
  if [ -f "$dir/Dockerfile" ] && ! grep -Eq 'distroless/static.*:nonroot|^USER +[^0 ]' "$dir/Dockerfile"; then
    echo "check-servers: $dir/Dockerfile does not run as a non-root user" >&2; fail=1
  fi
done
exit "$fail"
