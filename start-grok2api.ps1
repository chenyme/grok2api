param(
    [string]$ListenHost,
    [int]$Port,
    [int]$Workers,
    [string]$CustomCaFile,
    [switch]$NoSystemCa,
    [switch]$SkipPortCheck
)

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

function Get-PortListeners {
    param([Parameter(Mandatory = $true)][int]$LocalPort)

    $items = New-Object System.Collections.Generic.List[object]

    try {
        $connections = Get-NetTCPConnection -State Listen -LocalPort $LocalPort -ErrorAction Stop
        foreach ($connection in $connections) {
            $process = Get-Process -Id $connection.OwningProcess -ErrorAction SilentlyContinue
            $items.Add([PSCustomObject]@{
                Address = $connection.LocalAddress
                Port = $LocalPort
                PID = $connection.OwningProcess
                ProcessName = if ($process) { $process.ProcessName } else { '' }
            })
        }
        return $items
    }
    catch {
        foreach ($entry in (netstat -ano | Select-String 'LISTENING')) {
            $text = (($entry.Line -replace '\s+', ' ').Trim())
            $parts = $text -split ' '
            if ($parts.Count -lt 5) {
                continue
            }

            $local = $parts[1]
            $processId = $parts[-1]
            if ($local -match ':(\d+)$' -and [int]$Matches[1] -eq $LocalPort) {
                $process = Get-Process -Id ([int]$processId) -ErrorAction SilentlyContinue
                $items.Add([PSCustomObject]@{
                    Address = $local
                    Port = $LocalPort
                    PID = [int]$processId
                    ProcessName = if ($process) { $process.ProcessName } else { '' }
                })
            }
        }
        return $items
    }
}

$ProjectRoot = Split-Path -Path $MyInvocation.MyCommand.Path -Parent
Set-Location -LiteralPath $ProjectRoot

Import-SimpleDotEnv -Path (Join-Path $ProjectRoot '.env')

$GranianPath = Join-Path $ProjectRoot '.venv\Scripts\granian.exe'
if (-not (Test-Path -LiteralPath $GranianPath)) {
    throw "Granian not found: $GranianPath. Run dependency install first."
}

if (-not $PSBoundParameters.ContainsKey('ListenHost')) {
    $ListenHost = Get-EnvValue -Name 'SERVER_HOST' -Default '127.0.0.1'
}
if (-not $PSBoundParameters.ContainsKey('Port')) {
    $Port = Get-EnvIntValue -Name 'SERVER_PORT' -Default 8000
}
if (-not $PSBoundParameters.ContainsKey('Workers')) {
    $Workers = Get-EnvIntValue -Name 'SERVER_WORKERS' -Default 1
}

$env:PYTHONUTF8 = '1'
$env:PYTHONIOENCODING = 'utf-8'

if ($NoSystemCa.IsPresent) {
    $env:GROK2API_USE_SYSTEM_CA = 'false'
}
elseif (-not (Test-Path -LiteralPath 'Env:GROK2API_USE_SYSTEM_CA')) {
    $env:GROK2API_USE_SYSTEM_CA = 'true'
}

if ($PSBoundParameters.ContainsKey('CustomCaFile')) {
    $resolvedCa = (Resolve-Path -LiteralPath $CustomCaFile).Path
    $env:GROK2API_CUSTOM_CA_FILE = $resolvedCa
}
elseif (Test-Path -LiteralPath 'Env:GROK2API_CUSTOM_CA_FILE') {
    $resolvedCa = (Resolve-Path -LiteralPath $env:GROK2API_CUSTOM_CA_FILE).Path
    $env:GROK2API_CUSTOM_CA_FILE = $resolvedCa
}
else {
    Remove-Item -LiteralPath 'Env:GROK2API_CUSTOM_CA_FILE' -ErrorAction SilentlyContinue
}

if (-not $SkipPortCheck.IsPresent) {
    $listeners = @(Get-PortListeners -LocalPort $Port)
    if ($listeners.Count -gt 0) {
        Write-Host "Port $Port is already in use:" -ForegroundColor Yellow
        $listeners | Format-Table -AutoSize | Out-String | Write-Host
        throw "Stop the process using port $Port, or pass -Port with another value."
    }
}

Write-Host '========================================' -ForegroundColor Cyan
Write-Host 'Grok2API startup settings' -ForegroundColor Cyan
Write-Host '========================================' -ForegroundColor Cyan
Write-Host "Project root : $ProjectRoot"
Write-Host "Listen host  : $ListenHost"
Write-Host "Listen port  : $Port"
Write-Host "Workers      : $Workers"
Write-Host "Use system CA: $($env:GROK2API_USE_SYSTEM_CA)"
if ([string]::IsNullOrWhiteSpace($env:GROK2API_CUSTOM_CA_FILE)) {
    $customCaDisplay = '(not set)'
}
else {
    $customCaDisplay = $env:GROK2API_CUSTOM_CA_FILE
}
Write-Host "Custom CA    : $customCaDisplay"
Write-Host '========================================' -ForegroundColor Cyan

& $GranianPath --interface asgi --host $ListenHost --port $Port --workers $Workers main:app
exit $LASTEXITCODE
