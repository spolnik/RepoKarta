#!/usr/bin/env sh
set -eu

version=${1:-}
if ! printf '%s' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
    printf 'usage: %s VERSION [OUTPUT-DIRECTORY]\n' "$0" >&2
    exit 2
fi

repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
output_directory=${2:-"$repository_root/dist/release"}
package_name="repokarta-$version-macos-arm64"
archive_path="$output_directory/$package_name.tar.gz"
if [ ! -f "$archive_path" ]; then
    printf 'package archive does not exist: %s\n' "$archive_path" >&2
    exit 1
fi

smoke_root=$(mktemp -d "${TMPDIR:-/tmp}/repokarta-package-smoke.XXXXXX")
server_pid=
cleanup() {
    if [ -n "$server_pid" ] && kill -0 "$server_pid" 2>/dev/null; then
        kill "$server_pid" 2>/dev/null || true
        wait "$server_pid" 2>/dev/null || true
    fi
    rm -rf "$smoke_root"
}
trap cleanup EXIT HUP INT TERM

tar -C "$smoke_root" -xzf "$archive_path"
package_root="$smoke_root/$package_name"
repository_directory="$smoke_root/repositories"
data_directory="$smoke_root/data"
mkdir -p "$repository_directory" "$data_directory"
port=$(
    node -e 'const net=require("node:net");const s=net.createServer();s.listen(0,"127.0.0.1",()=>{process.stdout.write(String(s.address().port));s.close();});'
)
base_url="http://127.0.0.1:$port"

"$package_root/repokarta" serve \
    -listen "127.0.0.1:$port" \
    -data-dir "$data_directory" \
    -open=false \
    "$repository_directory" \
    >"$smoke_root/server.stdout.log" \
    2>"$smoke_root/server.stderr.log" &
server_pid=$!

ready=0
attempt=0
while [ "$attempt" -lt 120 ]; do
    if ! kill -0 "$server_pid" 2>/dev/null; then
        break
    fi
    health=$(
        curl --fail --silent --show-error --max-time 2 "$base_url/healthz" 2>/dev/null || true
    )
    if printf '%s' "$health" | grep -Fq '"status":"ok"' &&
        printf '%s' "$health" | grep -Fq "\"version\":\"$version\""; then
        ready=1
        break
    fi
    attempt=$((attempt + 1))
    sleep 0.25
done
if [ "$ready" -ne 1 ]; then
    cat "$smoke_root/server.stderr.log" >&2
    printf 'packaged server did not become healthy\n' >&2
    exit 1
fi

curl --fail --silent --show-error --max-time 5 "$base_url/" |
    grep -Fq '/assets/app.js'
asset_size=$(
    curl --fail --silent --show-error --max-time 5 "$base_url/assets/app.js" |
        wc -c | tr -d ' '
)
if [ "$asset_size" -lt 1000 ]; then
    printf 'packaged application asset was missing or unexpectedly small\n' >&2
    exit 1
fi

printf 'Package smoke passed for %s\n' "$archive_path"
