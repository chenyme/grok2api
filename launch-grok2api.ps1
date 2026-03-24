param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)
$OutputEncoding = [Console]::OutputEncoding

function Import-SimpleDotEnv {
    param([Parameter(Mandatory = $true)][string]$Path)

    if (-not (Test-Path -LiteralPath $Path)) {
        return
    }

    foreach ($rawLine in Get-Content -LiteralPath $Path -Encoding UTF8) {
        $line = $rawLine.Trim()
        if (-not $line -or $line.StartsWith('#')) {
            continue
        }

        $index = $line.IndexOf('=')
        if ($index -lt 1) {
            continue
        }

        $key = $line.Substring(0, $index).Trim()
        $value = $line.Substring($index + 1).Trim()

        if ($value.Length -ge 2) {
            if (($value.StartsWith('"') -and $value.EndsWith('"')) -or ($value.StartsWith("'") -and $value.EndsWith("'"))) {
                $value = $value.Substring(1, $value.Length - 2)
            }
        }

        if (-not (Test-Path -LiteralPath "Env:$key")) {
            Set-Item -LiteralPath "Env:$key" -Value $value
        }
    }
}

function Get-EnvValue {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [string]$Default = ''
    )

    $item = Get-Item -LiteralPath "Env:$Name" -ErrorAction SilentlyContinue
    if ($null -eq $item -or [string]::IsNullOrWhiteSpace($item.Value)) {
        return $Default
    }
    return [string]$item.Value
}

function Get-EnvIntValue {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [int]$Default
    )

    $raw = Get-EnvValue -Name $Name -Default ''
    if (-not $raw) {
        return $Default
    }

    $parsed = 0
    if ([int]::TryParse($raw, [ref]$parsed)) {
        return $parsed
    }

    return $Default
}

function Test-Health {
    param([Parameter(Mandatory = $true)][string]$Url)

    try {
        $null = Invoke-WebRequest -Uri $Url -UseBasicParsing -TimeoutSec 3
        return $true
    }
    catch {
        return $false
    }
}

$ProjectRoot = Split-Path -Path $MyInvocation.MyCommand.Path -Parent
Set-Location -LiteralPath $ProjectRoot
Import-SimpleDotEnv -Path (Join-Path $ProjectRoot '.env')

$port = Get-EnvIntValue -Name 'SERVER_PORT' -Default 8000
$listenHost = Get-EnvValue -Name 'SERVER_HOST' -Default '127.0.0.1'
$browserHost = switch ($listenHost.ToLowerInvariant()) {
    '0.0.0.0' { '127.0.0.1' }
    '::' { '127.0.0.1' }
    'localhost' { '127.0.0.1' }
    default { $listenHost }
}

$healthUrl = "http://127.0.0.1:$port/health"
$adminUrl = "http://${browserHost}:$port/admin/login"
$startScript = Join-Path $ProjectRoot 'start-grok2api.ps1'

Write-Host 'Checking Grok2API service...' -ForegroundColor Cyan
if (Test-Health -Url $healthUrl) {
    Write-Host 'Service is already running. Opening admin page...' -ForegroundColor Green
    Start-Process $adminUrl | Out-Null
    exit 0
}

Write-Host 'Starting Grok2API in a new PowerShell window...' -ForegroundColor Cyan
Start-Process powershell -WorkingDirectory $ProjectRoot -ArgumentList @(
    '-ExecutionPolicy', 'Bypass',
    '-NoProfile',
    '-File', $startScript
) | Out-Null

$ready = $false
for ($i = 0; $i -lt 30; $i++) {
    Start-Sleep -Seconds 1
    if (Test-Health -Url $healthUrl) {
        $ready = $true
        break
    }
}

if ($ready) {
    Write-Host 'Service started successfully. Opening admin page...' -ForegroundColor Green
    Start-Process $adminUrl | Out-Null
    exit 0
}

Write-Host ''
Write-Host 'Startup timed out.' -ForegroundColor Yellow
Write-Host 'Please check the newly opened PowerShell window for details.' -ForegroundColor Yellow
Write-Host "You can also visit: $adminUrl" -ForegroundColor Yellow
exit 1
