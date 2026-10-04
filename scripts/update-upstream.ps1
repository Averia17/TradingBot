param(
    [ValidateSet('trading-agents', 'nautilus-trader')]
    [string]$Repository = 'trading-agents',
    [string]$Release = '',
    [switch]$CheckOnly
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$taskRoot = Split-Path -Parent $PSScriptRoot
$taskCommand = if ($CheckOnly) { 'check' } else { 'update' }
$taskArguments = @('run', '--no-project', '--python', '3.12', 'python', 'scripts/upstream.py', $taskCommand, '--repo', $Repository)
if ($Release) { $taskArguments += @('--ref', $Release) }
Push-Location $taskRoot
try {
    & uv @taskArguments
    if ($LASTEXITCODE -ne 0) { throw 'Upstream update failed; inspect the error and restoration status above' }
} finally {
    Pop-Location
}
