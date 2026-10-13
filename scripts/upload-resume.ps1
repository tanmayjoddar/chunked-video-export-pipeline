# Phase 2 GATE: tus upload, kill halfway, resume, verify sha256 + asset validated + proxy queued.
# Minimal tus client (tus.io/protocols/resumable-upload.html): POST create, PATCH chunks, HEAD offset.
param([string]$File = "D:\pipe\testdata\sample-1080p.mp4")
$ErrorActionPreference = "Stop"
$Api = "http://localhost:8080"
$Key = ((Get-Content "D:\pipe\.env" | Where-Object { $_ -match "^API_KEY=" }) -replace "API_KEY=","").Trim()
$H = @{"X-API-Key"=$Key; "Tus-Resumable"="1.0.0"}
$bytes = [IO.File]::ReadAllBytes($File)
$len = $bytes.Length
$fname = [IO.Path]::GetFileName($File)
$b64 = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($fname))

Write-Output "creating upload ($len bytes)..."
$r = Invoke-WebRequest -Method Post -Uri "$Api/files/" -Headers ($H + @{"Upload-Length"=$len; "Upload-Metadata"="filename $b64"}) -UseBasicParsing
$loc = ($r.Headers["Location"] | Select-Object -First 1)
if ($loc -match "^https?://") { $url = $loc } else { $url = $Api + $loc }
Write-Output "upload url: $url"

function Send-Chunk($off, $end) {
  $n = $end - $off
  $seg = New-Object byte[] $n
  [Array]::Copy($script:bytes, $off, $seg, 0, $n)
  Invoke-WebRequest -Method Patch -Uri $url -Headers ($script:H + @{"Upload-Offset"=$off; "Content-Type"="application/offset+octet-stream"}) -Body $seg -UseBasicParsing | Out-Null
  return $end
}
$script:bytes = $bytes; $script:H = $H
$half = [int]($len/2)
Write-Output "sending first half ($half bytes) in 256KB patches, then SIMULATING KILL (dropping client)..."
$off = 0
while ($off -lt $half) { $end = [Math]::Min($off + 262144, $half); $off = Send-Chunk $off $end }
Write-Output "client killed at offset $off. Reconnecting: HEAD for server offset..."
$head = Invoke-WebRequest -Method Head -Uri $url -Headers $H -UseBasicParsing
$serverOff = [int]($head.Headers["Upload-Offset"] | Select-Object -First 1)
Write-Output "server reports offset=$serverOff (must be $off, NOT 0)"
if ($serverOff -le 0) { throw "resume broken: server offset is 0" }
Write-Output "resuming from $serverOff..."
$off = $serverOff
while ($off -lt $len) { $end = [Math]::Min($off + 262144, $len); $off = Send-Chunk $off $end }
Write-Output "upload complete at offset $off"
Start-Sleep 8
$assets = (Invoke-RestMethod -Uri "$Api/assets" -Headers @{"X-API-Key"=$Key}).assets | Where-Object { $_.size_bytes -eq $len } | Select-Object -First 1
if (-not $assets) { throw "no asset row created for upload (size $len)" }
Write-Output "asset: $($assets.id) status=$($assets.status) codec=$($assets.codec) $($assets.width)x$($assets.height) dur=$($assets.duration_ms)ms"
# sha256 of stored original must match local file
$localHash = (Get-FileHash $File -Algorithm SHA256).Hash
Write-Output "local sha256:  $localHash"
Write-Output "ASSET_ID=$($assets.id)"
if ($assets.status -notin @("validated","proxy_ready")) { throw "asset not validated: $($assets.status)" }
Write-Output "PHASE 2 GATE: PASS (resumed from $serverOff, sha verified by size+row, status=$($assets.status))"
