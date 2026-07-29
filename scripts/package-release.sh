#!/usr/bin/env sh
set -eu

version=${1:-}
if ! printf '%s' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
    printf 'usage: %s VERSION [OUTPUT-DIRECTORY]\n' "$0" >&2
    exit 2
fi

repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
output_directory=${2:-"$repository_root/dist/release"}
package_platform=${REPOKARTA_PACKAGE_PLATFORM:-macos-arm64}
case "$package_platform" in
    macos-arm64)
        target_os=darwin
        target_arch=arm64
        ;;
    linux-amd64)
        target_os=linux
        target_arch=amd64
        ;;
    linux-arm64)
        target_os=linux
        target_arch=arm64
        ;;
    *)
        printf 'unsupported package platform: %s\n' "$package_platform" >&2
        exit 2
        ;;
esac
package_name="repokarta-$version-$package_platform"
archive_path="$output_directory/$package_name.tar.gz"
checksum_path="$archive_path.sha256"
grammar_tags="grammar_subset,grammar_subset_bash,grammar_subset_go,grammar_subset_groovy,grammar_subset_java,grammar_subset_javascript,grammar_subset_kotlin,grammar_subset_python,grammar_subset_sql,grammar_subset_tsx,grammar_subset_typescript"
temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/repokarta-release.XXXXXX")
stage_directory="$temporary_root/$package_name"
verify_directory="$temporary_root/verify"
trap 'rm -rf "$temporary_root"' EXIT HUP INT TERM

mkdir -p "$output_directory" "$stage_directory/licenses"
mkdir -p "$stage_directory/docs" "$stage_directory/deploy"

if [ "${REPOKARTA_SKIP_VALIDATION:-}" != "1" ]; then
    npm --userconfig "$repository_root/.npmrc" --prefix "$repository_root/web" ci
    npm --prefix "$repository_root/web" test
    npm --prefix "$repository_root/web" run typecheck
    npm --prefix "$repository_root/web" run build
    (
        cd "$repository_root"
        go test -tags "$grammar_tags" ./...
    )
fi

(
    cd "$repository_root"
    GOOS="$target_os" GOARCH="$target_arch" CGO_ENABLED=0 \
    go build -buildvcs=false -tags "$grammar_tags" -trimpath \
        -ldflags "-s -w -X main.version=$version" \
        -o "$stage_directory/repokarta" ./cmd/repokarta
)
chmod 0755 "$stage_directory/repokarta"
cp "$repository_root/README.md" "$stage_directory/README.md"
cp "$repository_root/LICENSE" "$stage_directory/LICENSE"
cp "$repository_root/docs/shared-deployment.md" \
    "$stage_directory/docs/shared-deployment.md"
cp "$repository_root/docs/enterprise-administration.md" \
    "$stage_directory/docs/enterprise-administration.md"
cp "$repository_root/docs/postgresql.md" \
    "$stage_directory/docs/postgresql.md"
cp "$repository_root/docs/dependency-advisories.md" \
    "$stage_directory/docs/dependency-advisories.md"
cp "$repository_root/docs/scip-indexes.md" \
    "$stage_directory/docs/scip-indexes.md"
cp "$repository_root/docs/ast-search.md" \
    "$stage_directory/docs/ast-search.md"
cp "$repository_root/docs/advanced-search.md" \
    "$stage_directory/docs/advanced-search.md"
cp "$repository_root/docs/dependency-management.md" \
    "$stage_directory/docs/dependency-management.md"
cp "$repository_root/docs/opentelemetry.md" \
    "$stage_directory/docs/opentelemetry.md"
cp "$repository_root/docs/frontend-contracts.md" \
    "$stage_directory/docs/frontend-contracts.md"
cp "$repository_root/docs/reachability.md" \
    "$stage_directory/docs/reachability.md"
cp -R "$repository_root/deploy/." "$stage_directory/deploy/"
cp "$repository_root/third_party/zoekt/LICENSE" \
    "$stage_directory/licenses/zoekt-Apache-2.0.txt"
cp "$repository_root/third_party/zoekt/LICENSE" \
    "$stage_directory/licenses/scip-Apache-2.0.txt"
cp "$repository_root/third_party/zoekt/LICENSE" \
    "$stage_directory/licenses/sourcegraph-beaut-Apache-2.0.txt"
cp "$repository_root/third_party/licenses/deps.dev-semver-Apache-2.0.txt" \
    "$stage_directory/licenses/deps.dev-semver-Apache-2.0.txt"
cp "$repository_root/third_party/licenses/gotreesitter-MIT.txt" \
    "$stage_directory/licenses/gotreesitter-MIT.txt"
cp "$repository_root/third_party/licenses/tree-sitter-grammars-MIT.txt" \
    "$stage_directory/licenses/tree-sitter-grammars-MIT.txt"
cp "$repository_root/third_party/licenses/nvim-treesitter-Kotlin-query-NOTICE.txt" \
    "$stage_directory/licenses/nvim-treesitter-Kotlin-query-NOTICE.txt"
cp "$repository_root/third_party/licenses/crewjam-saml-BSD-2-Clause.txt" \
    "$stage_directory/licenses/crewjam-saml-BSD-2-Clause.txt"
cp "$repository_root/third_party/licenses/pgx-MIT.txt" \
    "$stage_directory/licenses/pgx-MIT.txt"

if [ "$target_os" = "darwin" ] && [ -n "${REPOKARTA_CODESIGN_IDENTITY:-}" ]; then
    codesign --force --options runtime --timestamp \
        --sign "$REPOKARTA_CODESIGN_IDENTITY" "$stage_directory/repokarta"
    codesign --verify --strict --verbose=2 "$stage_directory/repokarta"
fi

notary_values=0
for value in \
    "${APPLE_ID:-}" \
    "${APPLE_TEAM_ID:-}" \
    "${APPLE_APP_PASSWORD:-}"
do
    if [ -n "$value" ]; then
        notary_values=$((notary_values + 1))
    fi
done
if [ "$target_os" = "darwin" ] && [ "$notary_values" -ne 0 ] && [ "$notary_values" -ne 3 ]; then
    printf 'APPLE_ID, APPLE_TEAM_ID, and APPLE_APP_PASSWORD must be configured together\n' >&2
    exit 1
fi
if [ "$target_os" = "darwin" ] && [ "$notary_values" -eq 3 ]; then
    if [ -z "${REPOKARTA_CODESIGN_IDENTITY:-}" ]; then
        printf 'notarization requires REPOKARTA_CODESIGN_IDENTITY\n' >&2
        exit 1
    fi
    notary_archive="$temporary_root/$package_name-notarization.zip"
    ditto -c -k --keepParent "$stage_directory/repokarta" "$notary_archive"
    xcrun notarytool submit "$notary_archive" \
        --apple-id "$APPLE_ID" \
        --team-id "$APPLE_TEAM_ID" \
        --password "$APPLE_APP_PASSWORD" \
        --wait
fi

rm -f "$archive_path" "$checksum_path"
tar -C "$temporary_root" -czf "$archive_path" "$package_name"
if command -v shasum >/dev/null 2>&1; then
    hash=$(shasum -a 256 "$archive_path" | awk '{print $1}')
elif command -v sha256sum >/dev/null 2>&1; then
    hash=$(sha256sum "$archive_path" | awk '{print $1}')
else
    printf 'neither shasum nor sha256sum is available\n' >&2
    exit 1
fi
printf '%s  %s\n' "$hash" "$package_name.tar.gz" > "$checksum_path"

mkdir -p "$verify_directory"
tar -C "$verify_directory" -xzf "$archive_path"
test -f "$verify_directory/$package_name/licenses/zoekt-Apache-2.0.txt"
test -f "$verify_directory/$package_name/licenses/deps.dev-semver-Apache-2.0.txt"
test -f "$verify_directory/$package_name/licenses/scip-Apache-2.0.txt"
test -f "$verify_directory/$package_name/licenses/sourcegraph-beaut-Apache-2.0.txt"
test -f "$verify_directory/$package_name/licenses/pgx-MIT.txt"
test -f "$verify_directory/$package_name/LICENSE"
test -f "$verify_directory/$package_name/docs/shared-deployment.md"
test -f "$verify_directory/$package_name/docs/enterprise-administration.md"
test -f "$verify_directory/$package_name/docs/postgresql.md"
test -f "$verify_directory/$package_name/docs/dependency-advisories.md"
test -f "$verify_directory/$package_name/docs/scip-indexes.md"
test -f "$verify_directory/$package_name/docs/ast-search.md"
test -f "$verify_directory/$package_name/docs/advanced-search.md"
test -f "$verify_directory/$package_name/docs/dependency-management.md"
test -f "$verify_directory/$package_name/docs/opentelemetry.md"
test -f "$verify_directory/$package_name/docs/frontend-contracts.md"
test -f "$verify_directory/$package_name/docs/reachability.md"
test -f "$verify_directory/$package_name/deploy/repokarta.env.example"
test -f "$verify_directory/$package_name/deploy/otel/collector-debug.yaml"
test -f "$verify_directory/$package_name/deploy/otel/collector-datadog.yaml"
test -f "$verify_directory/$package_name/deploy/otel/datadog-agent.yaml"
if { [ "$target_os" = "darwin" ] && [ "$(uname -s)" = "Darwin" ]; } ||
    { [ "$target_os" = "linux" ] && [ "$(uname -s)" = "Linux" ]; }; then
    reported_version=$("$verify_directory/$package_name/repokarta" version)
    test "$reported_version" = "$version"
fi

printf 'Packaged %s\nChecksum %s\n' "$archive_path" "$checksum_path"
