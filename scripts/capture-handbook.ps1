[CmdletBinding()]
param(
    [string]$Address = "127.0.0.1:7332",
    [switch]$SkipWebBuild
)

$ErrorActionPreference = "Stop"

$repositoryRoot = Split-Path -Parent $PSScriptRoot
$handbookRoot = Join-Path $repositoryRoot "handbook"
$workspaceRoot = Split-Path -Parent $repositoryRoot
$demoRoot = Join-Path $handbookRoot ".demo-workspace"
$binaryDirectory = Join-Path $handbookRoot ".capture-bin"
$dataDirectory = Join-Path $handbookRoot ".capture-data"
$cacheDirectory = Join-Path $handbookRoot ".capture-cache"
$logDirectory = Join-Path $dataDirectory "logs"
$binaryPath = Join-Path $binaryDirectory "repokarta-handbook.exe"
$stdoutPath = Join-Path $logDirectory "repokarta.stdout.log"
$stderrPath = Join-Path $logDirectory "repokarta.stderr.log"

foreach ($directory in @(
    $demoRoot,
    $binaryDirectory,
    $dataDirectory,
    $cacheDirectory,
    $logDirectory,
    (Join-Path $handbookRoot "src\assets\captures"),
    (Join-Path $handbookRoot "public\media\captures")
)) {
    New-Item -ItemType Directory -Force -Path $directory | Out-Null
}

function Initialize-DemoClone {
    param(
        [Parameter(Mandatory)]
        [string]$Name,
        [Parameter(Mandatory)]
        [string]$Source,
        [Parameter(Mandatory)]
        [string]$Revision
    )

    $destination = Join-Path $demoRoot $Name
    $resolvedDemoRoot = [System.IO.Path]::GetFullPath($demoRoot)
    $resolvedDestination = [System.IO.Path]::GetFullPath($destination)
    if (-not $resolvedDestination.StartsWith(
        $resolvedDemoRoot + [System.IO.Path]::DirectorySeparatorChar,
        [System.StringComparison]::OrdinalIgnoreCase
    )) {
        throw "Demo repository path escaped the handbook workspace: $resolvedDestination"
    }

    if (-not (Test-Path -LiteralPath (Join-Path $destination ".git"))) {
        git clone --shared --no-checkout $Source $destination
        if ($LASTEXITCODE -ne 0) {
            throw "Could not create the local demo clone for $Name"
        }
    }

    git -C $destination checkout --detach $Revision
    if ($LASTEXITCODE -ne 0) {
        throw "Could not select $Revision for $Name"
    }
}

$repoKartaRevision = (git -C $repositoryRoot rev-parse HEAD).Trim()
if (-not $repoKartaRevision) {
    throw "Could not resolve the RepoKarta revision"
}

Initialize-DemoClone `
    -Name "RepoKarta" `
    -Source $repositoryRoot `
    -Revision $repoKartaRevision
Initialize-DemoClone `
    -Name "spring-petclinic-microservices" `
    -Source (Join-Path $workspaceRoot "spring-petclinic-microservices") `
    -Revision "305a1f13e4f961001d4e6cb50a9db51dc3fc5967"
Initialize-DemoClone `
    -Name "bank-of-anthos" `
    -Source (Join-Path $workspaceRoot "bank-of-anthos") `
    -Revision "1e40564f9ff572a28281198903e19da93e506770"

if (-not $SkipWebBuild) {
    npm --prefix (Join-Path $repositoryRoot "web") run build
    if ($LASTEXITCODE -ne 0) {
        throw "RepoKarta browser build failed"
    }
}

$previousGoCache = $env:GOCACHE
$env:GOCACHE = $cacheDirectory
try {
    go build -o $binaryPath ./cmd/repokarta
    if ($LASTEXITCODE -ne 0) {
        throw "RepoKarta capture binary build failed"
    }
}
finally {
    $env:GOCACHE = $previousGoCache
}

$process = $null
try {
    $process = Start-Process `
        -FilePath $binaryPath `
        -ArgumentList @(
            "serve",
            "-open=false",
            "-listen=$Address",
            "-data-dir=$dataDirectory",
            $demoRoot
        ) `
        -WorkingDirectory $repositoryRoot `
        -WindowStyle Hidden `
        -RedirectStandardOutput $stdoutPath `
        -RedirectStandardError $stderrPath `
        -PassThru

    $baseURL = "http://$Address"
    $deadline = [DateTimeOffset]::UtcNow.AddMinutes(2)
    do {
        if ($process.HasExited) {
            throw "RepoKarta stopped before the capture began. See $stderrPath"
        }
        try {
            $response = Invoke-WebRequest `
                -UseBasicParsing `
                -Uri "$baseURL/api/repositories" `
                -TimeoutSec 2
            if ($response.StatusCode -eq 200) {
                break
            }
        }
        catch {
            Start-Sleep -Milliseconds 400
        }
    } while ([DateTimeOffset]::UtcNow -lt $deadline)

    if ([DateTimeOffset]::UtcNow -ge $deadline) {
        throw "RepoKarta did not become ready at $baseURL"
    }

    $env:REPOKARTA_CAPTURE_URL = $baseURL
    npm --prefix $handbookRoot run capture
    if ($LASTEXITCODE -ne 0) {
        throw "Handbook browser capture failed"
    }

    $manifest = [ordered]@{
        captured_at = [DateTimeOffset]::UtcNow.ToString("o")
        repokarta = $repoKartaRevision
        repositories = [ordered]@{
            "spring-petclinic-microservices" = "305a1f13e4f961001d4e6cb50a9db51dc3fc5967"
            "bank-of-anthos" = "1e40564f9ff572a28281198903e19da93e506770"
        }
        viewport = "1600x900 at 1.2x device scale"
        media = [ordered]@{
            walkthroughs = "1920x1080 VP9 WebM, CRF 20"
            stills = "1440x900 PNG"
        }
        browser = "Playwright Chromium; walkthrough frames encoded with FFmpeg"
        walkthroughs = @(
            "search-overview",
            "source-browser",
            "repository-map"
        )
        stills = @(
            "dependencies-topology",
            "dependency-inventory",
            "insights-overview",
            "wiki-workspace",
            "chat-workspace",
            "mcp-setup"
        )
        captures = @(
            "search-overview",
            "source-browser",
            "repository-map",
            "dependencies-topology",
            "dependency-inventory",
            "insights-overview",
            "wiki-workspace",
            "chat-workspace",
            "mcp-setup"
        )
    }
    $manifest |
        ConvertTo-Json -Depth 4 |
        Set-Content -LiteralPath (Join-Path $handbookRoot "capture-manifest.json") -Encoding utf8
}
finally {
    Remove-Item Env:\REPOKARTA_CAPTURE_URL -ErrorAction SilentlyContinue
    if ($process -and -not $process.HasExited) {
        Stop-Process -Id $process.Id
        $process.WaitForExit()
    }
}
