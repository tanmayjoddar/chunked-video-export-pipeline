# Requires: Docker Desktop running. Run from repo root.
$ErrorActionPreference = "Stop"
if (-not (Test-Path ".env")) { Copy-Item ".env.example" ".env"; Write-Output "created .env from example - EDIT API_KEY first!" }
docker compose up -d --build
Write-Output "waiting for api..."
for ($i=0; $i -lt 30; $i++) {
  try { $r = Invoke-WebRequest -Uri "http://localhost:8080/healthz" -TimeoutSec 3; if ($r.StatusCode -eq 200) { Write-Output "healthz 200 OK"; break } } catch { Start-Sleep 2 }
}
Invoke-WebRequest -Uri "http://localhost:8080/healthz" | Select-Object StatusCode, Content
docker compose logs --tail=20 api
