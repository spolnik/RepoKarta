$ErrorActionPreference = "Stop"

$repositoryRoot = Split-Path -Parent $PSScriptRoot
$outputDirectory = Join-Path $repositoryRoot "dist"
$licenseDirectory = Join-Path $outputDirectory "licenses"

npm --prefix (Join-Path $repositoryRoot "web") install
if ($LASTEXITCODE -ne 0) { throw "npm install failed" }

npm --prefix (Join-Path $repositoryRoot "web") run typecheck
if ($LASTEXITCODE -ne 0) { throw "frontend typecheck failed" }

npm --prefix (Join-Path $repositoryRoot "web") run build
if ($LASTEXITCODE -ne 0) { throw "frontend build failed" }

Push-Location $repositoryRoot
try {
    go test ./...
    if ($LASTEXITCODE -ne 0) { throw "Go tests failed" }

    New-Item -ItemType Directory -Force -Path $licenseDirectory | Out-Null
    go build -trimpath -o (Join-Path $outputDirectory "repokarta.exe") ./cmd/repokarta
    if ($LASTEXITCODE -ne 0) { throw "Go build failed" }

    Copy-Item `
        -LiteralPath (Join-Path $repositoryRoot "third_party\zoekt\LICENSE") `
        -Destination (Join-Path $licenseDirectory "zoekt-Apache-2.0.txt") `
        -Force
}
finally {
    Pop-Location
}

Write-Host "Built $outputDirectory\repokarta.exe with third-party licenses"
