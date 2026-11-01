param(
    [string]$Tag = 'latest',
    [string]$Repo = 'lucaswilliameufrasio/multi-source-repo-sync',
    [string]$BinDir = (Join-Path $env:LOCALAPPDATA 'multi-source-repo-sync\bin'),
    [switch]$Force
)

$ErrorActionPreference = 'Stop'

$arch = if ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture -eq 'Arm64') { 'arm64' } else { 'amd64' }
if ($Tag -eq 'latest') {
    $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest"
    $Tag = $release.tag_name
}
$version = $Tag.TrimStart('v')
$asset = "multi-source-repo-sync_${version}_windows_${arch}.zip"
$baseUrl = "https://github.com/$Repo/releases/download/$Tag"
$tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ([System.IO.Path]::GetRandomFileName())
$archive = Join-Path $tempDir $asset
$checksumFile = "$archive.sha256"

try {
    New-Item -ItemType Directory -Path $tempDir | Out-Null
    Write-Host "Downloading $baseUrl/$asset"
    Invoke-WebRequest -Uri "$baseUrl/$asset" -OutFile $archive
    Invoke-WebRequest -Uri "$baseUrl/$asset.sha256" -OutFile $checksumFile

    $expected = (Get-Content $checksumFile -Raw).Trim() -split '\s+' | Select-Object -First 1
    $actual = (Get-FileHash -Algorithm SHA256 -Path $archive).Hash.ToLowerInvariant()
    if ($expected.ToLowerInvariant() -ne $actual) { throw "Checksum mismatch for $asset" }
    Write-Host 'Checksum verified'

    Expand-Archive -Path $archive -DestinationPath $tempDir -Force
    $binary = Join-Path $tempDir 'multi-source-repo-sync.exe'
    if (-not (Test-Path $binary)) { throw 'Archive does not contain multi-source-repo-sync.exe' }
    if ((Test-Path (Join-Path $BinDir 'multi-source-repo-sync.exe')) -and -not $Force) {
        $answer = Read-Host "Replace $(Join-Path $BinDir 'multi-source-repo-sync.exe')? [y/N]"
        if ($answer -notmatch '^[Yy]$') { throw 'Installation cancelled' }
    }
    New-Item -ItemType Directory -Path $BinDir -Force | Out-Null
    Copy-Item $binary (Join-Path $BinDir 'multi-source-repo-sync.exe') -Force
    Write-Host "Installed multi-source-repo-sync $Tag to $(Join-Path $BinDir 'multi-source-repo-sync.exe')"
    Write-Host "If needed, add this directory to PATH: $BinDir"
} finally {
    Remove-Item $tempDir -Recurse -Force -ErrorAction SilentlyContinue
}
