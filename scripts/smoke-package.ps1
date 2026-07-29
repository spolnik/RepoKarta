param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$')]
    [string]$Version,

    [string]$OutputDirectory
)

$ErrorActionPreference = "Stop"

$repositoryRoot = [IO.Path]::GetFullPath((Split-Path -Parent $PSScriptRoot))
if ([string]::IsNullOrWhiteSpace($OutputDirectory)) {
    $OutputDirectory = Join-Path $repositoryRoot "dist\release"
}
$OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory)
$packageName = "repokarta-$Version-windows-amd64"
$archivePath = Join-Path $OutputDirectory "$packageName.zip"
if (-not (Test-Path -LiteralPath $archivePath -PathType Leaf)) {
    throw "package archive does not exist: $archivePath"
}

$temporaryRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
$smokeRoot = Join-Path $temporaryRoot ("repokarta-package-smoke-" + [guid]::NewGuid().ToString("N"))
$smokeRoot = [IO.Path]::GetFullPath($smokeRoot)
if (
    -not $smokeRoot.StartsWith($temporaryRoot, [StringComparison]::OrdinalIgnoreCase) -or
    -not [IO.Path]::GetFileName($smokeRoot).StartsWith("repokarta-package-smoke-", [StringComparison]::Ordinal)
) {
    throw "package smoke directory escaped the operating-system temporary directory"
}

$listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
$listener.Start()
$port = ([Net.IPEndPoint]$listener.LocalEndpoint).Port
$listener.Stop()

$process = $null
try {
    New-Item -ItemType Directory -Path $smokeRoot | Out-Null
    Expand-Archive -LiteralPath $archivePath -DestinationPath $smokeRoot
    $packageRoot = Join-Path $smokeRoot $packageName
    $executable = Join-Path $packageRoot "repokarta.exe"
    $repositoryDirectory = Join-Path $smokeRoot "repositories"
    $dataDirectory = Join-Path $smokeRoot "data"
    New-Item -ItemType Directory -Path $repositoryDirectory, $dataDirectory | Out-Null
    $stdoutPath = Join-Path $smokeRoot "server.stdout.log"
    $stderrPath = Join-Path $smokeRoot "server.stderr.log"
    $process = Start-Process `
        -FilePath $executable `
        -ArgumentList @(
            "serve",
            "-listen", "127.0.0.1:$port",
            "-data-dir", $dataDirectory,
            "-open=false",
            $repositoryDirectory
        ) `
        -WindowStyle Hidden `
        -RedirectStandardOutput $stdoutPath `
        -RedirectStandardError $stderrPath `
        -PassThru

    $baseURL = "http://127.0.0.1:$port"
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    $ready = $false
    while ([DateTime]::UtcNow -lt $deadline) {
        if ($process.HasExited) {
            break
        }
        try {
            $health = Invoke-RestMethod -Uri "$baseURL/healthz" -TimeoutSec 2
            if ($health.status -eq "ok" -and $health.version -eq $Version) {
                $ready = $true
                break
            }
        }
        catch {
            Start-Sleep -Milliseconds 250
        }
    }
    if (-not $ready) {
        $stderr = if (Test-Path -LiteralPath $stderrPath) {
            Get-Content -LiteralPath $stderrPath -Raw
        } else {
            ""
        }
        throw "packaged server did not become healthy: $stderr"
    }

    $homeResponse = Invoke-WebRequest -UseBasicParsing -Uri "$baseURL/" -TimeoutSec 5
    if ($homeResponse.StatusCode -ne 200 -or $homeResponse.Content -notmatch "/assets/app\.js") {
        throw "packaged home page did not reference the embedded application asset"
    }
    $asset = Invoke-WebRequest -UseBasicParsing -Uri "$baseURL/assets/app.js" -TimeoutSec 5
    if ($asset.StatusCode -ne 200 -or $asset.RawContentLength -lt 1000) {
        throw "packaged application asset was missing or unexpectedly small"
    }
    Write-Host "Package smoke passed for $archivePath"
}
finally {
    if ($null -ne $process -and -not $process.HasExited) {
        Stop-Process -Id $process.Id -Force
        $null = $process.WaitForExit(5000)
    }
    if (
        (Test-Path -LiteralPath $smokeRoot) -and
        $smokeRoot.StartsWith($temporaryRoot, [StringComparison]::OrdinalIgnoreCase) -and
        [IO.Path]::GetFileName($smokeRoot).StartsWith("repokarta-package-smoke-", [StringComparison]::Ordinal)
    ) {
        Remove-Item -LiteralPath $smokeRoot -Recurse -Force
    }
}
