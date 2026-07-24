#!/usr/bin/env sh
set -eu

repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
output_directory="$repository_root/dist"
license_directory="$output_directory/licenses"

npm --prefix "$repository_root/web" install
npm --prefix "$repository_root/web" run typecheck
npm --prefix "$repository_root/web" run build

cd "$repository_root"
go test ./...
mkdir -p "$license_directory"
go build -trimpath -o "$output_directory/repokarta" ./cmd/repokarta
cp "$repository_root/third_party/zoekt/LICENSE" \
    "$license_directory/zoekt-Apache-2.0.txt"

printf 'Built %s with third-party licenses\n' "$output_directory/repokarta"
