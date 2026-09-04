<#
.SYNOPSIS
  Bring the whole farm up on this machine, with no Docker and no phones.

.DESCRIPTION
  Starts a THROWAWAY PostgreSQL cluster on a private port, applies the schema,
  and runs the demo: simulated hardware driven by the real scheduler, lease
  store, reaper and recovery ladder.

  It never touches an existing PostgreSQL service. The cluster lives under
  -DataDir, listens only on 127.0.0.1, and uses trust auth because nothing
  outside this machine can reach it. Delete the directory to reset everything.

.EXAMPLE
  .\scripts\dev-up.ps1
  .\scripts\dev-up.ps1 -Devices 120 -Hosts 4
  .\scripts\dev-up.ps1 -Reset          # start from an empty database
#>
[CmdletBinding()]
param(
  [int]    $Port     = 55432,
  [int]    $ApiPort  = 8420,
  [int]    $Hosts    = 2,
  [int]    $Devices  = 56,
  [string] $DataDir  = "$env:LOCALAPPDATA\device-farmer\pgdata",
  [string] $PgBin    = "",
  [switch] $Reset,
  [switch] $NoDemo
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot

function Find-PgBin {
  if ($PgBin) { return $PgBin }
  $candidates = Get-ChildItem 'C:\Program Files\PostgreSQL' -Directory -ErrorAction SilentlyContinue |
                Sort-Object { [int]($_.Name -replace '\D','0') } -Descending
  foreach ($c in $candidates) {
    if (Test-Path (Join-Path $c.FullName 'bin\initdb.exe')) { return (Join-Path $c.FullName 'bin') }
  }
  throw "PostgreSQL binaries not found. Install PostgreSQL 16+ or pass -PgBin."
}

$bin    = Find-PgBin
$initdb = Join-Path $bin 'initdb.exe'
$pgctl  = Join-Path $bin 'pg_ctl.exe'
$psql   = Join-Path $bin 'psql.exe'
$log    = Join-Path (Split-Path -Parent $DataDir) 'postgres.log'

if ($Reset -and (Test-Path $DataDir)) {
  Write-Host "resetting the throwaway cluster at $DataDir" -ForegroundColor Yellow
  & $pgctl -D $DataDir stop -m immediate 2>$null | Out-Null
  Remove-Item -Recurse -Force $DataDir
}

if (-not (Test-Path $DataDir)) {
  Write-Host "creating a throwaway cluster at $DataDir" -ForegroundColor Cyan
  New-Item -ItemType Directory -Force -Path (Split-Path -Parent $DataDir) | Out-Null
  & $initdb -D $DataDir -U farm -A trust -E UTF8 --locale=C | Out-Null
}

# Already listening? Reuse it rather than fighting over the port.
$listening = Test-NetConnection -ComputerName 127.0.0.1 -Port $Port -InformationLevel Quiet -WarningAction SilentlyContinue
if (-not $listening) {
  Write-Host "starting postgres on 127.0.0.1:$Port" -ForegroundColor Cyan
  & $pgctl -D $DataDir -o "-p $Port -c listen_addresses=127.0.0.1" -l $log start | Out-Null
  for ($i = 0; $i -lt 30; $i++) {
    try { & $psql -h 127.0.0.1 -p $Port -U farm -d postgres -tAc 'select 1' 2>$null | Out-Null; break }
    catch { Start-Sleep -Milliseconds 300 }
  }
} else {
  Write-Host "postgres already listening on $Port; reusing it" -ForegroundColor DarkGray
}

$exists = & $psql -h 127.0.0.1 -p $Port -U farm -d postgres -tAc `
  "select 1 from pg_database where datname='device_farmer'"
if (-not $exists) {
  & $psql -h 127.0.0.1 -p $Port -U farm -d postgres -q -c 'CREATE DATABASE device_farmer' | Out-Null
}

$env:DATABASE_URL = "postgres://farm@127.0.0.1:$Port/device_farmer?sslmode=disable"
$env:FARM_API_ADDR = "127.0.0.1:$ApiPort"

Write-Host 'building farmd' -ForegroundColor Cyan
Push-Location $repo
try {
  & go build -trimpath -o 'bin\farmd.exe' './cmd/farmd'
  if ($LASTEXITCODE -ne 0) { throw 'build failed' }

  # The schema is embedded in the binary, so this works from any directory.
  & '.\bin\farmd.exe' migrate up
  if ($LASTEXITCODE -ne 0) { throw 'migration failed' }

  if ($NoDemo) {
    Write-Host ''
    Write-Host "Database ready: $env:DATABASE_URL" -ForegroundColor Green
    Write-Host 'Start a role yourself, for example:  .\bin\farmd.exe all'
    return
  }

  Write-Host ''
  Write-Host "dashboard: http://127.0.0.1:$ApiPort/" -ForegroundColor Green
  Write-Host 'Ctrl-C stops the demo. Live leases are intentionally NOT released.' -ForegroundColor DarkGray
  Write-Host ''
  & '.\bin\farmd.exe' demo -hosts $Hosts -devices $Devices
}
finally { Pop-Location }
