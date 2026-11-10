#Requires -Version 5.1
<#
.SYNOPSIS
    Cross-build the SV Multi-Agent CLI (svma) for a chosen operating system.

.DESCRIPTION
    Produces a portable static Go binary that works as a command-line tool:
        svma serve | submit | get | list | wait | setup | config | version

    The sv-multi-agent service and all agents run in-process; there is no Python runtime.

    Run without parameters for an interactive menu. For automation pass -Target.

.PARAMETER Target
    e.g. linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64,
    windows/arm64, or "all". If omitted, an interactive menu is shown.

.PARAMETER All
    Also build the legacy standalone sv-multi-agent and svctl binaries.

.PARAMETER Version
    Version string embedded into the binary (default: git describe or "dev").

.PARAMETER OutDir
    Output directory (default: <repo>/dist).

.PARAMETER Go
    Path to the go executable (default: "go" from PATH).

.EXAMPLE
    .\build.ps1
    .\build.ps1 -Target linux/amd64
    .\build.ps1 -Target all -All
#>
[CmdletBinding()]
param(
    [string]$Target,
    [switch]$All,
    [string]$Version,
    [string]$OutDir,
    [string]$Go = "go"
)

$ErrorActionPreference = "Stop"

$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Definition
$repoRoot = Split-Path -Parent $scriptDir
if (-not $OutDir) { $OutDir = Join-Path $repoRoot "dist" }

$choices = [ordered]@{
    "1" = "linux/amd64"
    "2" = "linux/arm64"
    "3" = "darwin/amd64"
    "4" = "darwin/arm64"
    "5" = "windows/amd64"
    "6" = "windows/arm64"
}

function Show-Menu {
    Write-Host ""
    Write-Host "SV Multi-Agent - cross-platform build"
    Write-Host "-------------------------------------"
    Write-Host "  1) linux/amd64"
    Write-Host "  2) linux/arm64"
    Write-Host "  3) darwin/amd64  (Intel macOS)"
    Write-Host "  4) darwin/arm64  (Apple Silicon)"
    Write-Host "  5) windows/amd64"
    Write-Host "  6) windows/arm64"
    Write-Host "  7) all targets"
    Write-Host "  q) quit"
    Write-Host ""
}

if (-not (Get-Command $Go -ErrorAction SilentlyContinue)) {
    throw "Go was not found. Install it (https://go.dev/dl/) or pass -Go <path-to-go.exe>."
}

if (-not $Version) {
    $Version = "dev"
    if (Get-Command git -ErrorAction SilentlyContinue) {
        $previous = $ErrorActionPreference
        $ErrorActionPreference = "Continue"
        $output = & git -C $repoRoot describe --tags --always 2>&1 | Out-String
        $gitExit = $LASTEXITCODE
        $ErrorActionPreference = $previous
        if ($gitExit -eq 0 -and $output.Trim()) {
            $Version = $output.Trim()
        }
    }
}

$selected = @()
if ($Target) {
    if ($Target -eq "all") {
        $selected = @($choices.Values)
    } else {
        $selected = @($Target)
    }
} else {
    Show-Menu
    $choice = Read-Host "Select target [1-7, q]"
    switch ($choice) {
        "q" { Write-Host "Cancelled."; return }
        "7" { $selected = @($choices.Values) }
        default {
            if ($choices.Contains($choice)) {
                $selected = @($choices[$choice])
            } elseif ($choice -match '^[A-Za-z0-9_]+/[A-Za-z0-9_]+$') {
                $selected = @($choice)
            } else {
                throw "Unknown choice: $choice"
            }
        }
    }
}

New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

$savedGOOS = $env:GOOS
$savedGOARCH = $env:GOARCH
$savedCGO = $env:CGO_ENABLED
Push-Location $repoRoot
try {
    foreach ($t in $selected) {
        $parts = $t.Split("/")
        if ($parts.Count -ne 2) { throw "Invalid target '$t' (expected OS/ARCH)" }
        $targetOS = $parts[0]
        $targetArch = $parts[1]
        $ext = if ($targetOS -eq "windows") { ".exe" } else { "" }

        $env:GOOS = $targetOS
        $env:GOARCH = $targetArch
        $env:CGO_ENABLED = "0"

        $svma = Join-Path $OutDir "svma-$targetOS-$targetArch$ext"
        Write-Host "Building $([System.IO.Path]::GetFileName($svma)) ..."
        & $Go build -trimpath -ldflags "-s -w -X main.version=$Version" -o $svma ./cmd/svma
        if ($LASTEXITCODE -ne 0) { throw "svma build failed for $t" }

        if ($All) {
            & $Go build -trimpath -ldflags "-s -w" -o (Join-Path $OutDir "sv-multi-agent-$targetOS-$targetArch$ext") ./cmd/sv-multi-agent
            if ($LASTEXITCODE -ne 0) { throw "sv-multi-agent build failed for $t" }
            & $Go build -trimpath -ldflags "-s -w" -o (Join-Path $OutDir "svctl-$targetOS-$targetArch$ext") ./cmd/svctl
            if ($LASTEXITCODE -ne 0) { throw "svctl build failed for $t" }
        }
    }
} finally {
    Pop-Location
    $env:GOOS = $savedGOOS
    $env:GOARCH = $savedGOARCH
    $env:CGO_ENABLED = $savedCGO
}

Write-Host ""
Write-Host "Done (version $Version)."
Write-Host "Binaries in: $OutDir"
Get-ChildItem $OutDir | Select-Object Name, Length | Format-Table -AutoSize
