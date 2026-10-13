# Phase 4 GATE: POST /exports -> plan -> chunks -> stitch; verify frame count, duration, hash.
param([string]$AssetId = "")
$ErrorActionPreference = "Stop"
$Api = "http://localhost:8080"
$Key = ((Get-Content "D:\pipe\.env" | Where-Object { $_ -match "^API_KEY=" }) -replace "API_KEY=","").Trim()
$H = @{ "X-API-Key" = $Key; "Content-Type" = "application/json" }
if (-not $AssetId) {
  $assets = (Invoke-RestMethod -Uri "$Api/assets" -Headers @{"X-API-Key"=$Key}).assets | Where-Object { $_.status -in @("validated","proxy_ready") } | Select-Object -First 1
  if (-not $assets) { throw "no validated asset; run upload-resume.ps1 first" }
  $AssetId = $assets.id
}
Write-Output "asset: $AssetId"
$body = @{ asset_id = $AssetId } | ConvertTo-Json
$exp = Invoke-RestMethod -Method Post -Uri "$Api/exports" -Headers $H -Body $body
$id = $exp.id
Write-Output "export: $id"
$t = 0
while ($t -lt 600) {
  $e = Invoke-RestMethod -Uri "$Api/exports/$id" -Headers @{"X-API-Key"=$Key}
  Write-Output "status=$($e.status) done=$($e.done)/$($e.chunk_count) pct=$([math]::Round($e.percent,1))"
  if ($e.status -eq "done") { break }
  if ($e.status -eq "failed") { throw "export failed: $($e | ConvertTo-Json -Compress)" }
  Start-Sleep 5; $t += 5
}
$e = Invoke-RestMethod -Uri "$Api/exports/$id" -Headers @{"X-API-Key"=$Key}
if ($e.status -ne "done") { throw "export not done in time" }
# verify inside api container (ffmpeg pinned there)
$out = docker compose exec -T postgres psql -U app -d exports -tAc "SELECT output_path FROM exports WHERE id='$id'"
$orig = docker compose exec -T postgres psql -U app -d exports -tAc "SELECT original_path FROM assets WHERE id='$AssetId'"
$fo = docker compose exec -T api ffprobe -v error -select_streams v:0 -count_frames -show_entries stream=nb_read_frames -of csv=p=0 "$out"
$fs = docker compose exec -T api ffprobe -v error -select_streams v:0 -count_frames -show_entries stream=nb_read_frames -of csv=p=0 "$orig"
$do = docker compose exec -T api ffprobe -v error -show_entries format=duration -of csv=p=0 "$out"
$ds = docker compose exec -T api ffprobe -v error -show_entries format=duration -of csv=p=0 "$orig"
$hash = docker compose exec -T api sha256sum "$out"
Write-Output "source frames=$fs duration=$ds"
Write-Output "output frames=$fo duration=$do"
Write-Output "output $hash"
Write-Output "EXPORT_ID=$id"
if ([int]$fo -ne [int]$fs) { throw "FRAME COUNT MISMATCH: $fo != $fs" }
if ([math]::Abs([double]$do - [double]$ds) -gt 0.15) { throw "DURATION DRIFT: $do vs $ds" }
Write-Output "PHASE 4 GATE: PASS"
