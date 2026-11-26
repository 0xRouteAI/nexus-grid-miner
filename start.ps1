# NEXUS GRID miner agent - one-click native start.
# Reads .env in this folder. TEI must already be running (see RUN_WINDOWS.md).
# Requires a Go toolchain (go 1.24+) on PATH. Builds the binary on every start.
# Several GPUs: give each process its own NODE_ID and TEI endpoints in .env.
param(
    [string]$Wallet = "",
    [string]$ServerUrl = ""
)

$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$agentRoot = Join-Path $root "agent-go"

# ---- load .env ----
if (Test-Path "$root\.env") {
    Get-Content "$root\.env" | ForEach-Object {
        $line = $_.Trim()
        if ($line -and -not $line.StartsWith('#')) {
            $parts = $line -split '=', 2
            if ($parts.Count -eq 2 -and $parts[0].Trim()) {
                Set-Item -Path ("Env:" + $parts[0].Trim()) -Value $parts[1].Trim()
            }
        }
    }
}

# ---- per-instance overrides ----
if ($Wallet)    { $env:WALLET = $Wallet }
if ($ServerUrl) { $env:SERVER_URL = $ServerUrl }

if (-not $env:SERVER_URL)        { throw "SERVER_URL is not set (.env or -ServerUrl), e.g. nexus.example.com" }
if (-not $env:WALLET)            { throw "WALLET is not set (.env or -Wallet)" }
if (-not $env:MINER_PRIVATE_KEY) { throw "MINER_PRIVATE_KEY is not set in .env" }

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "go not found on PATH. Install Go 1.24+ (https://go.dev/dl/) and reopen the shell."
}

# ---- build ----
$bin = Join-Path $agentRoot "bin"
New-Item -ItemType Directory -Force -Path $bin | Out-Null
$exe = Join-Path $bin "nexus-miner-agent.exe"
Write-Host "building miner agent..."
Push-Location $agentRoot
try {
    go build -trimpath -o $exe ./cmd/agent
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }
} finally {
    Pop-Location
}

# ---- start one process per wallet ----
$logs = Join-Path $root "logs"
New-Item -ItemType Directory -Force -Path $logs | Out-Null
$tag = $env:WALLET.Substring([Math]::Max(0, $env:WALLET.Length - 4))
$out = Join-Path $logs ("agent-$tag.out.log")
$err = Join-Path $logs ("agent-$tag.err.log")

$running = Get-Process "nexus-miner-agent" -ErrorAction SilentlyContinue
if ($running) {
    Write-Host "note: $($running.Count) miner agent(s) already running; starting another one is fine if the wallet differs"
}

Start-Process -FilePath $exe -WorkingDirectory $agentRoot -WindowStyle Hidden `
    -RedirectStandardOutput $out -RedirectStandardError $err | Out-Null

Write-Host "miner agent started"
Write-Host "wallet:  $env:WALLET"
Write-Host "server:  $env:SERVER_URL"
Write-Host "logs:    $out / $err"
