$ErrorActionPreference = "Stop"

$repositoryRoot = Split-Path -Parent $PSScriptRoot
$outputDirectory = Join-Path $repositoryRoot "dist"
$licenseDirectory = Join-Path $outputDirectory "licenses"
$gitMarkerPath = Join-Path $repositoryRoot ".git"
$worktreeGitDirectory = $null
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

if (Test-Path -LiteralPath $gitMarkerPath) {
    $gitMarker = Get-Item -Force -LiteralPath $gitMarkerPath
    if (-not $gitMarker.PSIsContainer) {
        $worktreeGitDirectory = git -C $repositoryRoot rev-parse --absolute-git-dir
        if ($LASTEXITCODE -ne 0) { throw "Could not resolve linked worktree Git directory" }
        $worktreeGitDirectory = $worktreeGitDirectory.Trim()
    }
}

npm --prefix (Join-Path $repositoryRoot "web") ci
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

    New-Item -ItemType Directory -Force -Path $licenseDirectory | Out-Null
    $previousGitDirectory = [Environment]::GetEnvironmentVariable("GIT_DIR", "Process")
    $previousGitWorkTree = [Environment]::GetEnvironmentVariable("GIT_WORK_TREE", "Process")
    try {
        if ($worktreeGitDirectory) {
            [Environment]::SetEnvironmentVariable("GIT_DIR", $worktreeGitDirectory, "Process")
            [Environment]::SetEnvironmentVariable("GIT_WORK_TREE", $repositoryRoot, "Process")
        }
        go build -tags $grammarTags -trimpath -o (Join-Path $outputDirectory "repokarta.exe") ./cmd/repokarta
        if ($LASTEXITCODE -ne 0) { throw "Go build failed" }
    }
    finally {
        [Environment]::SetEnvironmentVariable("GIT_DIR", $previousGitDirectory, "Process")
        [Environment]::SetEnvironmentVariable("GIT_WORK_TREE", $previousGitWorkTree, "Process")
    }

    Copy-Item `
        -LiteralPath (Join-Path $repositoryRoot "third_party\zoekt\LICENSE") `
        -Destination (Join-Path $licenseDirectory "zoekt-Apache-2.0.txt") `
        -Force
    Copy-Item `
        -LiteralPath (Join-Path $repositoryRoot "third_party\zoekt\LICENSE") `
        -Destination (Join-Path $licenseDirectory "deps.dev-semver-Apache-2.0.txt") `
        -Force
    Copy-Item `
        -LiteralPath (Join-Path $repositoryRoot "third_party\licenses\gotreesitter-MIT.txt") `
        -Destination (Join-Path $licenseDirectory "gotreesitter-MIT.txt") `
        -Force
    Copy-Item `
        -LiteralPath (Join-Path $repositoryRoot "third_party\licenses\tree-sitter-grammars-MIT.txt") `
        -Destination (Join-Path $licenseDirectory "tree-sitter-grammars-MIT.txt") `
        -Force
    Copy-Item `
        -LiteralPath (Join-Path $repositoryRoot "third_party\licenses\nvim-treesitter-Kotlin-query-NOTICE.txt") `
        -Destination (Join-Path $licenseDirectory "nvim-treesitter-Kotlin-query-NOTICE.txt") `
        -Force
    Copy-Item `
        -LiteralPath (Join-Path $repositoryRoot "third_party\licenses\crewjam-saml-BSD-2-Clause.txt") `
        -Destination (Join-Path $licenseDirectory "crewjam-saml-BSD-2-Clause.txt") `
        -Force
}
finally {
    Pop-Location
}

Write-Host "Built $outputDirectory\repokarta.exe with third-party licenses"
