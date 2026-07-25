#!/usr/bin/env sh
set -eu

repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
output_directory="$repository_root/dist"
license_directory="$output_directory/licenses"
grammar_tags="grammar_subset,grammar_subset_bash,grammar_subset_go,grammar_subset_groovy,grammar_subset_java,grammar_subset_javascript,grammar_subset_kotlin,grammar_subset_python,grammar_subset_sql,grammar_subset_tsx,grammar_subset_typescript"

npm --prefix "$repository_root/web" install
npm --prefix "$repository_root/web" run typecheck
npm --prefix "$repository_root/web" run build

cd "$repository_root"
go test -tags "$grammar_tags" ./...
mkdir -p "$license_directory"
go build -tags "$grammar_tags" -trimpath -o "$output_directory/repokarta" ./cmd/repokarta
cp "$repository_root/third_party/zoekt/LICENSE" \
    "$license_directory/zoekt-Apache-2.0.txt"
cp "$repository_root/third_party/licenses/gotreesitter-MIT.txt" \
    "$license_directory/gotreesitter-MIT.txt"
cp "$repository_root/third_party/licenses/tree-sitter-grammars-MIT.txt" \
    "$license_directory/tree-sitter-grammars-MIT.txt"
cp "$repository_root/third_party/licenses/nvim-treesitter-Kotlin-query-NOTICE.txt" \
    "$license_directory/nvim-treesitter-Kotlin-query-NOTICE.txt"
cp "$repository_root/third_party/licenses/crewjam-saml-BSD-2-Clause.txt" \
    "$license_directory/crewjam-saml-BSD-2-Clause.txt"

printf 'Built %s with third-party licenses\n' "$output_directory/repokarta"
