# scripts/bench.ps1 — measure 1/2/4 workers on same 1080p input.
param([string]$AssetId = "")
$ErrorActionPreference = "Stop"
$Key = $env:API_KEY
if (-not $Key) { $Key = ((Get-Content .env | Where-Object { $_ -match "^API_KEY=" }) -replace "API_KEY=","") }
$H = @{ "X-API-Key" = $Key; "Content-Type" = "application/json" }
$Api = "http://localhost:8080"
if (-not $AssetId) { throw "pass -AssetId <uuid>" }
$body = @{ asset_id = $AssetId } | ConvertTo-Json
$table = @()
foreach ($w in @(1,2,4)) {
  docker compose up -d --scale worker=$w worker | Out-Null
  Start-Sleep 5
  $sw = [Diagnostics.Stopwatch]::StartNew()
  $exp = Invoke-RestMethod -Method Post -Uri "$Api/exports" -Headers $H -Body $body
  $t=0
  while ($t -lt 900) {
    $e = Invoke-RestMethod -Uri "$Api/exports/$($exp.id)" -Headers @{"X-API-Key"=$Key}
    if ($e.status -eq "done") { break }
    if ($e.status -eq "failed") { throw "export failed" }
    Start-Sleep 3; $t+=3
  }
  $sw.Stop()
  $table += [pscustomobject]@{ workers=$w; seconds=[math]::Round($sw.Elapsed.TotalSeconds,1); export=$exp.id }
  Write-Output "workers=$w seconds=$([math]::Round($sw.Elapsed.TotalSeconds,1))"
}
$table | Format-Table -AutoSize
