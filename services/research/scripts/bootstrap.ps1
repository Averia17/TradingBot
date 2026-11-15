$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$taskRoot = Split-Path -Parent $PSScriptRoot
Push-Location $taskRoot
try {
    & uv run --no-project --python 3.12 python scripts/upstream.py bootstrap
    if ($LASTEXITCODE -ne 0) { throw 'Clean upstream verification failed' }
    & uv sync --frozen --extra evaluation
    if ($LASTEXITCODE -ne 0) { throw 'Dependency installation failed' }
    & uv run --frozen --extra evaluation tradingbot doctor
    if ($LASTEXITCODE -ne 0) { throw 'Component check failed' }
} finally {
    Pop-Location
}
