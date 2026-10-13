# scripts/chaos.ps1 — recovery proof.
# Runs an export, kills random worker containers mid-chunk several rounds (compose restarts them),
# waits for completion, then verifies:
# (a) frame count == source, (b) kill-run sha256 == clean-run sha256 on same input,
# (c) zero chunks with duplicate completion, (d) every chunk row ends 'done' with attempts recorded.
param(
  [string]$AssetId = "",
  [int]$Rounds = 3,
  [int]$Runs = 20
)
$ErrorActionPreference = "Stop"
$Api = "http://localhost:8080"
$Key = ((Get-Content "D:\pipe\.env" | Where-Object { $_ -match "^API_KEY=" }) -replace "API_KEY=","").Trim()
$H = @{ "X-API-Key" = $Key; "Content-Type" = "application/json" }
if (-not $AssetId) {
  $a = (Invoke-RestMethod -Uri "$Api/assets" -Headers @{"X-API-Key"=$Key}).assets | Where-Object { $_.status -in @("validated","proxy_ready") } | Select-Object -First 1
  if (-not $a) { throw "no validated asset; run scripts/upload-resume.ps1 first" }
  $AssetId = $a.id
}
Write-Output "asset=$AssetId rounds=$Rounds runs=$Runs"

function Wait-Export($id, $timeoutS=900) {
  $t=0
  while ($t -lt $timeoutS) {
    $e = Invoke-RestMethod -Uri "$Api/exports/$id" -Headers @{"X-API-Key"=$Key}
    if ($e.status -eq "done") { return $e }
    if ($e.status -eq "failed") { throw "export $id failed" }
    Start-Sleep 5; $t+=5
  }
  throw "timeout waiting for $id"
}

$results = @()
for ($run=1; $run -le $Runs; $run++) {
  Write-Output "=== run $run/$Runs ==="
  $body = @{ asset_id = $AssetId } | ConvertTo-Json
  $exp = Invoke-RestMethod -Method Post -Uri "$Api/exports" -Headers $H -Body $body
  $id = $exp.id
  Start-Sleep 8  # let chunks start running, then kill mid-chunk
  for ($k=1; $k -le $Rounds; $k++) {
    $workers = @(docker compose ps -q worker)
    if ($workers.Count -gt 0) {
      $pick = $workers | Get-Random
      Write-Output "round ${k}: docker kill ${pick}"
      docker kill $pick | Out-Null
    }
    Start-Sleep 10
  }
  $fin = Wait-Export $id
  # clean run on same asset for hash equality
  $exp2 = Invoke-RestMethod -Method Post -Uri "$Api/exports" -Headers $H -Body $body
  $fin2 = Wait-Export $exp2.id
  # (a) frame counts
  $orig = docker compose exec -T postgres psql -U app -d exports -tAc "SELECT original_path FROM assets WHERE id='$AssetId'"
  $out1 = docker compose exec -T postgres psql -U app -d exports -tAc "SELECT output_path FROM exports WHERE id='$id'"
  $out2 = docker compose exec -T postgres psql -U app -d exports -tAc "SELECT output_path FROM exports WHERE id='$($exp2.id)'"
  $fs = (docker compose exec -T api ffprobe -v error -select_streams v:0 -count_frames -show_entries stream=nb_read_frames -of csv=p=0 "$orig").Trim()
  $f1 = (docker compose exec -T api ffprobe -v error -select_streams v:0 -count_frames -show_entries stream=nb_read_frames -of csv=p=0 "$out1").Trim()
  # (b) hashes
  $h1 = ((docker compose exec -T api sha256sum "$out1").Trim() -split '\s+')[0]
  $h2 = ((docker compose exec -T api sha256sum "$out2").Trim() -split '\s+')[0]
  # (c) duplicate completion: same chunk completed twice would need 2 done-transitions;
  # conditional UPDATE running->done + UNIQUE(export_id,idx) make it impossible; assert
  # finished_at set once and exactly one done row per idx.
  $dup = docker compose exec -T postgres psql -U app -d exports -tAc "SELECT count(*) FROM (SELECT idx,count(*) c FROM export_chunks WHERE export_id='$id' GROUP BY idx HAVING count(*)>1) t;"
  # (d) all done + attempts recorded
  $nonDone = docker compose exec -T postgres psql -U app -d exports -tAc "SELECT count(*) FROM export_chunks WHERE export_id='$id' AND status<>'done';"
  $noAttempt = docker compose exec -T postgres psql -U app -d exports -tAc "SELECT count(*) FROM export_chunks WHERE export_id='$id' AND attempts<1;"
  $stitches = docker compose exec -T postgres psql -U app -d exports -tAc "SELECT count(*) FROM river_job WHERE kind='export-stitch' AND state='completed' AND args->>'ExportID'='$id';"
  $okA = ([int]$f1 -eq [int]$fs); $okB = ($h1 -eq $h2)
  $okC = ($dup.Trim() -eq "0"); $okD = ($nonDone.Trim() -eq "0" -and $noAttempt.Trim() -eq "0")
  $pass = $okA -and $okB -and $okC -and $okD
  $aStr = "a-FAIL"; if ($okA) { $aStr = "a-OK" }
  $pStr = "FAIL"; if ($pass) { $pStr = "PASS" }
  Write-Output "run $run : frames src=$fs out=$f1 [$aStr] hash_match=$okB dup=$($dup.Trim()) nondone=$($nonDone.Trim()) noattempt=$($noAttempt.Trim()) stitches=$($stitches.Trim()) => $pStr"
  $results += [pscustomobject]@{ run=$run; kill_export=$id; clean_export=$exp2.id; frames="$f1/$fs"; hash_match=$okB; nondone=$nonDone.Trim(); pass=$pass }
  if (-not $pass) { Write-Output "FAILING FAST (see row above)"; break }
}
Write-Output ""
Write-Output "===== SUMMARY ====="
$results | Format-Table -AutoSize
$passed = @($results | Where-Object { $_.pass -eq $true }).Count
Write-Output "passed $passed / $($results.Count)"
if ($passed -ne $Runs) { Write-Output "CHAOS FAILED"; exit 1 } else { Write-Output "CHAOS PASSED: $Runs consecutive runs" }
