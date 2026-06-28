$ErrorActionPreference = "Stop"
Set-Location -LiteralPath $PSScriptRoot

$dockerDir = Join-Path $PSScriptRoot "docker"
$certPath = Join-Path $dockerDir "ca-certificates.crt"
$binaryPath = Join-Path $dockerDir "bridge"
New-Item -ItemType Directory -Force -Path $dockerDir | Out-Null

if (-not (Test-Path -LiteralPath $certPath)) {
    $certSource = python -c "import certifi; print(certifi.where())"
    if (-not $certSource -or -not (Test-Path -LiteralPath $certSource)) {
        throw "Unable to locate a CA certificate bundle through Python certifi."
    }
    Copy-Item -LiteralPath $certSource -Destination $certPath
}

$oldGOOS = $env:GOOS
$oldGOARCH = $env:GOARCH
$oldCGO = $env:CGO_ENABLED
try {
    $env:GOOS = "linux"
    $env:GOARCH = "amd64"
    $env:CGO_ENABLED = "0"
    go build -buildvcs=false -trimpath -ldflags="-s -w" -o $binaryPath .
} finally {
    $env:GOOS = $oldGOOS
    $env:GOARCH = $oldGOARCH
    $env:CGO_ENABLED = $oldCGO
}

docker build -f Dockerfile.local -t blackbox-minimax-bridge:local .
