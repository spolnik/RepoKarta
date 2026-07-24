#!/usr/bin/env sh
set -eu

repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
output_directory="$repository_root/dist"

npm --prefix "$repository_root/web" install
npm --prefix "$repository_root/web" run typecheck
npm --prefix "$repository_root/web" run build

cd "$repository_root"
go test ./...
mkdir -p "$output_directory"
go build -trimpath -o "$output_directory/repokarta" ./cmd/repokarta

printf 'Built %s\n' "$output_directory/repokarta"
