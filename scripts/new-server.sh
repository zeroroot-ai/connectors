#!/usr/bin/env bash
# Start a new server from _template. Usage: scripts/new-server.sh <name>
set -euo pipefail
name="${1:-}"
if ! [[ "$name" =~ ^[a-z][a-z0-9-]{0,40}[a-z0-9]$ ]]; then
  echo "usage: new-server.sh <name>   (lowercase letters, digits and hyphens)" >&2
  exit 2
fi
root="$(cd "$(dirname "$0")/.." && pwd)"
dest="$root/$name"
if [ -e "$dest" ]; then echo "new-server: $dest exists" >&2; exit 1; fi
prefix="$(echo "$name" | tr 'a-z-' 'A-Z_')"
mkdir "$dest"
for f in "$root"/_template/*; do
  sed -e "s/__NAME__/$name/g" -e "s/__ENVPREFIX__/$prefix/g" "$f" > "$dest/$(basename "$f")"
done
echo "created $dest"
echo "next: edit $name/server.go, cite the vendor API doc in a comment, write the tests, state the vendor permission in $name/README.md"
