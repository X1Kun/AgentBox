#!/usr/bin/env bash
set -euo pipefail

# Project-local install; preserves the runsc sidecar layout and does not touch
# Docker configuration or /usr/local/bin. Pass a dated release to reproduce it.
project_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
release=${1:-20261005.0}
arch=$(uname -m)
case "$arch" in x86_64|aarch64) ;; *) echo "Unsupported architecture: $arch" >&2; exit 1;; esac
if [[ ! "$release" =~ ^(latest|[0-9]{8}(\.[0-9]+)?)$ ]]; then
    echo "Release must be latest, YYYYMMDD, or YYYYMMDD.N" >&2
    exit 1
fi
destination="$project_dir/.local/gvisor"
if [[ -e "$destination" ]]; then
    echo "Already exists: $destination; keeping the current installation" >&2
    exit 1
fi
mkdir -p "$project_dir/.local"
temporary_dir=$(mktemp -d "$project_dir/.local/runsc-download.XXXXXXXX")
trap 'rm -rf -- "$temporary_dir"' EXIT
base_url="https://storage.googleapis.com/gvisor/releases/release/$release/$arch"
curl --fail --location --retry 2 --connect-timeout 15 --max-time 180 \
    "$base_url/gvisor.tar.bz2" -o "$temporary_dir/gvisor.tar.bz2"
curl --fail --location --retry 2 --connect-timeout 15 --max-time 60 \
    "$base_url/gvisor.tar.bz2.sha512" -o "$temporary_dir/gvisor.tar.bz2.sha512"
(cd "$temporary_dir" && sha512sum --check gvisor.tar.bz2.sha512)
mkdir "$temporary_dir/install"
tar -xjf "$temporary_dir/gvisor.tar.bz2" -C "$temporary_dir/install" --no-same-owner
test -x "$temporary_dir/install/runsc"
"$temporary_dir/install/runsc" --version > "$temporary_dir/install/VERSION.txt"
cp "$temporary_dir/gvisor.tar.bz2.sha512" "$temporary_dir/install/ARCHIVE.sha512"
printf '%s\n' "$base_url" > "$temporary_dir/install/SOURCE.txt"
mv "$temporary_dir/install" "$destination"
cat "$destination/VERSION.txt"
