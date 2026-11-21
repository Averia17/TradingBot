[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string]$Output,
    [string[]]$Symbols = @("NVDA", "TSLA", "AAPL", "MSFT"),
    [string]$SecUserAgent = $env:TRADINGBOT_SEC_USER_AGENT,
    [string]$CashUSD = "1000"
)

$ErrorActionPreference = "Stop"
$research = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path
$control = (Resolve-Path -LiteralPath (Join-Path $research "../control")).Path
$outputRoot = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Output)
$secPath = Join-Path $outputRoot "sec-archive.json"
$newsPath = Join-Path $outputRoot "issuer-news.json"
$prepared = Join-Path $outputRoot "snapshot"

if (!$SecUserAgent) {
    $SecUserAgent = "TradingBot local research/1.0"
}
New-Item -Path $outputRoot -ItemType Directory | Out-Null
go -C $control run -mod=readonly ./cmd/research-sec-capture --symbols ($Symbols -join ",") --user-agent $SecUserAgent --out $secPath
if ($LASTEXITCODE -ne 0) {
    throw "SEC capture failed"
}
$secPin = (Get-FileHash -LiteralPath $secPath -Algorithm SHA256).Hash.ToLowerInvariant()
$sources = @("--source", $secPath, $secPin)

# Tesla's company events are collected through its SEC filings.
$symsWithFeeds = @($Symbols | Where-Object { $_ -in @("NVDA", "AAPL", "MSFT") })
if ($symsWithFeeds.Count -gt 0) {
    $issuerNews = go -C $control run -mod=readonly ./cmd/research-news-capture --symbols ($symsWithFeeds -join ",")
    if ($LASTEXITCODE -ne 0) {
        throw "Issuer news capture failed"
    }
    [IO.File]::WriteAllLines($newsPath, [string[]]$issuerNews, [Text.UTF8Encoding]::new($false))
    $newsPin = (Get-FileHash -LiteralPath $newsPath -Algorithm SHA256).Hash.ToLowerInvariant()
    $sources += @("--source", $newsPath, $newsPin)
}

uv --directory $research run --offline --frozen tradingbot prepare-research --symbols @Symbols @sources --cash-usd $CashUSD --output $prepared
if ($LASTEXITCODE -ne 0) {
    throw "Research snapshot preparation failed"
}
