param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$')]
    [string]$Version,

    [string]$OutputDirectory,

    [switch]$SkipValidation
)

$ErrorActionPreference = "Stop"

$repositoryRoot = [IO.Path]::GetFullPath((Split-Path -Parent $PSScriptRoot))
if ([string]::IsNullOrWhiteSpace($OutputDirectory)) {
    $OutputDirectory = Join-Path $repositoryRoot "dist\release"
}
$OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory)
$packageName = "repokarta-$Version-windows-amd64"
$stageRoot = Join-Path $OutputDirectory ".stage"
$stageDirectory = Join-Path $stageRoot $packageName
$archivePath = Join-Path $OutputDirectory "$packageName.zip"
$checksumPath = "$archivePath.sha256"
$outputPrefix = $OutputDirectory.TrimEnd([IO.Path]::DirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
$stagePrefix = [IO.Path]::GetFullPath($stageDirectory)
if (-not $stagePrefix.StartsWith($outputPrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw "release staging directory escaped the configured output directory"
}

$grammarTags = @(
    "grammar_subset",
    "grammar_subset_bash",
    "grammar_subset_go",
    "grammar_subset_groovy",
    "grammar_subset_java",
    "grammar_subset_javascript",
    "grammar_subset_kotlin",
    "grammar_subset_python",
    "grammar_subset_sql",
    "grammar_subset_tsx",
    "grammar_subset_typescript"
) -join ","

function Copy-ThirdPartyLicenses {
    param([string]$Destination)
    New-Item -ItemType Directory -Force -Path $Destination | Out-Null
    $licenses = [ordered]@{
        "third_party\zoekt\LICENSE" = "zoekt-Apache-2.0.txt"
        "third_party\licenses\gotreesitter-MIT.txt" = "gotreesitter-MIT.txt"
        "third_party\licenses\tree-sitter-grammars-MIT.txt" = "tree-sitter-grammars-MIT.txt"
        "third_party\licenses\nvim-treesitter-Kotlin-query-NOTICE.txt" = "nvim-treesitter-Kotlin-query-NOTICE.txt"
        "third_party\licenses\crewjam-saml-BSD-2-Clause.txt" = "crewjam-saml-BSD-2-Clause.txt"
        "third_party\licenses\pgx-MIT.txt" = "pgx-MIT.txt"
    }
    foreach ($source in $licenses.Keys) {
        Copy-Item -LiteralPath (Join-Path $repositoryRoot $source) -Destination (Join-Path $Destination $licenses[$source]) -Force
    }
    Copy-Item `
        -LiteralPath (Join-Path $repositoryRoot "third_party\licenses\deps.dev-semver-Apache-2.0.txt") `
        -Destination (Join-Path $Destination "deps.dev-semver-Apache-2.0.txt") `
        -Force
    Copy-Item `
        -LiteralPath (Join-Path $repositoryRoot "third_party\zoekt\LICENSE") `
        -Destination (Join-Path $Destination "scip-Apache-2.0.txt") `
        -Force
    Copy-Item `
        -LiteralPath (Join-Path $repositoryRoot "third_party\zoekt\LICENSE") `
        -Destination (Join-Path $Destination "sourcegraph-beaut-Apache-2.0.txt") `
        -Force
}

New-Item -ItemType Directory -Force -Path $OutputDirectory | Out-Null
if (Test-Path -LiteralPath $stageDirectory) {
    Remove-Item -LiteralPath $stageDirectory -Recurse -Force
}
New-Item -ItemType Directory -Force -Path $stageDirectory | Out-Null

try {
    if (-not $SkipValidation) {
        npm --userconfig (Join-Path $repositoryRoot ".npmrc") --prefix (Join-Path $repositoryRoot "web") ci
        if ($LASTEXITCODE -ne 0) { throw "npm ci failed" }
        npm --prefix (Join-Path $repositoryRoot "web") test
        if ($LASTEXITCODE -ne 0) { throw "frontend tests failed" }
        npm --prefix (Join-Path $repositoryRoot "web") run typecheck
        if ($LASTEXITCODE -ne 0) { throw "frontend typecheck failed" }
        npm --prefix (Join-Path $repositoryRoot "web") run build
        if ($LASTEXITCODE -ne 0) { throw "frontend build failed" }

        Push-Location $repositoryRoot
        try {
            go test -tags $grammarTags ./...
            if ($LASTEXITCODE -ne 0) { throw "Go tests failed" }
        }
        finally {
            Pop-Location
        }
    }

    Push-Location $repositoryRoot
    try {
        $env:GOOS = "windows"
        $env:GOARCH = "amd64"
        $env:CGO_ENABLED = "0"
        go build -buildvcs=false -tags $grammarTags -trimpath -ldflags "-s -w -X main.version=$Version" -o (Join-Path $stageDirectory "repokarta.exe") ./cmd/repokarta
        if ($LASTEXITCODE -ne 0) { throw "Windows build failed" }
    }
    finally {
        Remove-Item Env:GOOS -ErrorAction SilentlyContinue
        Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
        Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue
        Pop-Location
    }

    Copy-Item -LiteralPath (Join-Path $repositoryRoot "README.md") -Destination (Join-Path $stageDirectory "README.md") -Force
    Copy-Item -LiteralPath (Join-Path $repositoryRoot "LICENSE") -Destination (Join-Path $stageDirectory "LICENSE") -Force
    New-Item -ItemType Directory -Force -Path (Join-Path $stageDirectory "docs") | Out-Null
    New-Item -ItemType Directory -Force -Path (Join-Path $stageDirectory "deploy") | Out-Null
    Copy-Item -LiteralPath (Join-Path $repositoryRoot "docs\shared-deployment.md") -Destination (Join-Path $stageDirectory "docs\shared-deployment.md") -Force
    Copy-Item -LiteralPath (Join-Path $repositoryRoot "docs\enterprise-administration.md") -Destination (Join-Path $stageDirectory "docs\enterprise-administration.md") -Force
    Copy-Item -LiteralPath (Join-Path $repositoryRoot "docs\postgresql.md") -Destination (Join-Path $stageDirectory "docs\postgresql.md") -Force
    Copy-Item -LiteralPath (Join-Path $repositoryRoot "docs\dependency-advisories.md") -Destination (Join-Path $stageDirectory "docs\dependency-advisories.md") -Force
    Copy-Item -LiteralPath (Join-Path $repositoryRoot "docs\scip-indexes.md") -Destination (Join-Path $stageDirectory "docs\scip-indexes.md") -Force
    Copy-Item -LiteralPath (Join-Path $repositoryRoot "docs\ast-search.md") -Destination (Join-Path $stageDirectory "docs\ast-search.md") -Force
    Copy-Item -LiteralPath (Join-Path $repositoryRoot "docs\advanced-search.md") -Destination (Join-Path $stageDirectory "docs\advanced-search.md") -Force
    Copy-Item -LiteralPath (Join-Path $repositoryRoot "docs\dependency-management.md") -Destination (Join-Path $stageDirectory "docs\dependency-management.md") -Force
    Copy-Item -LiteralPath (Join-Path $repositoryRoot "docs\opentelemetry.md") -Destination (Join-Path $stageDirectory "docs\opentelemetry.md") -Force
    Copy-Item -Path (Join-Path $repositoryRoot "deploy\*") -Destination (Join-Path $stageDirectory "deploy") -Recurse -Force
    Copy-ThirdPartyLicenses -Destination (Join-Path $stageDirectory "licenses")

    $reportedVersion = & (Join-Path $stageDirectory "repokarta.exe") version
    if ($LASTEXITCODE -ne 0 -or $reportedVersion.Trim() -ne $Version) {
        throw "packaged executable reports '$reportedVersion', expected '$Version'"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $stageDirectory "licenses\zoekt-Apache-2.0.txt"))) {
        throw "packaged executable is missing the full Zoekt Apache-2.0 license"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $stageDirectory "licenses\deps.dev-semver-Apache-2.0.txt"))) {
        throw "packaged executable is missing the deps.dev semver Apache-2.0 license"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $stageDirectory "licenses\scip-Apache-2.0.txt"))) {
        throw "packaged executable is missing the SCIP Apache-2.0 license"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $stageDirectory "licenses\sourcegraph-beaut-Apache-2.0.txt"))) {
        throw "packaged executable is missing the Sourcegraph beaut Apache-2.0 license"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $stageDirectory "licenses\pgx-MIT.txt"))) {
        throw "packaged executable is missing the pgx MIT license"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $stageDirectory "LICENSE"))) {
        throw "package is missing the RepoKarta Apache-2.0 license"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $stageDirectory "docs\shared-deployment.md"))) {
        throw "package is missing the shared-deployment runbook"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $stageDirectory "docs\enterprise-administration.md"))) {
        throw "package is missing the enterprise-administration runbook"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $stageDirectory "docs\postgresql.md"))) {
        throw "package is missing the PostgreSQL runbook"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $stageDirectory "docs\dependency-advisories.md"))) {
        throw "package is missing the dependency-advisories runbook"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $stageDirectory "docs\scip-indexes.md"))) {
        throw "package is missing the SCIP index runbook"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $stageDirectory "docs\ast-search.md"))) {
        throw "package is missing the structural AST search guide"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $stageDirectory "docs\advanced-search.md"))) {
        throw "package is missing the advanced search guide"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $stageDirectory "docs\dependency-management.md"))) {
        throw "package is missing the dependency management guide"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $stageDirectory "docs\opentelemetry.md"))) {
        throw "package is missing the OpenTelemetry operations guide"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $stageDirectory "deploy\otel\collector-debug.yaml"))) {
        throw "package is missing the OpenTelemetry Collector debug configuration"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $stageDirectory "deploy\otel\collector-datadog.yaml"))) {
        throw "package is missing the OpenTelemetry Collector Datadog configuration"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $stageDirectory "deploy\otel\datadog-agent.yaml"))) {
        throw "package is missing the Datadog Agent OTLP configuration"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $stageDirectory "deploy\repokarta.env.example"))) {
        throw "package is missing the shared-deployment environment template"
    }

    if (Test-Path -LiteralPath $archivePath) {
        Remove-Item -LiteralPath $archivePath -Force
    }
    Compress-Archive -LiteralPath $stageDirectory -DestinationPath $archivePath -CompressionLevel Optimal
    $hash = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
    Set-Content -LiteralPath $checksumPath -Value "$hash  $packageName.zip" -Encoding ascii
}
finally {
    if (Test-Path -LiteralPath $stageDirectory) {
        Remove-Item -LiteralPath $stageDirectory -Recurse -Force
    }
}

Write-Host "Packaged $archivePath"
Write-Host "Checksum $checksumPath"
