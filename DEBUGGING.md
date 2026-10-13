# DEBUGGING.md

Format: Symptom -> likely cause -> exact PowerShell command to confirm -> fix.

## Upload resumes from 0 instead of resuming

Likely cause: tusd store path / volume not persisted (new empty dir each boot), so the server has no record of the partial upload.

```powershell
# server offset should be > 0 after partial upload:
$Key = ((Get-Content .env | Where-Object { $_ -match "^API_KEY=" }) -replace "API_KEY=","").Trim()
Invoke-WebRequest -Method Head -Uri "http://localhost:8080/files/<upload-id>" -Headers @{"X-API-Key"=$Key;"Tus-Resumable"="1.0.0"} -UseBasicParsing | Select-Object -ExpandProperty Headers
docker compose exec -T api ls -la /data/tusd | head
docker volume ls | Select-String mediadata
```

Fix: keep the `mediadata:/data` volume (do not `docker compose down -v` unless you mean to wipe uploads); `TUSD_DIR` must stay `/data/tusd`.

## Chunk stuck in 'running' forever

Likely cause: worker died mid-chunk (OOM, `docker kill`, `fatal error: newosproc` from too-low `pids_limit`).

```powershell
docker compose logs -f worker
docker compose exec -T postgres psql -U app -d exports -c "SELECT export_id,idx,attempts,worker_id,now()-heartbeat_at AS idle FROM export_chunks WHERE status='running';"
docker compose exec -T postgres psql -U app -d exports -c "SELECT id,kind,state,attempt FROM river_job WHERE state='running' ORDER BY id DESC LIMIT 10;"
```

Fix: nothing manual needed. River rescues jobs running longer than `RESCUE_STUCK_AFTER_S` (rescuer ticks every 30s; see `river@v0.17.0/internal/maintenance/job_rescuer.go`: `JobRescuerIntervalDefault = 30s`), and the worker's watchdog releases DB chunks with stale `heartbeat_at` back to `pending`:

```powershell
docker compose exec -T postgres psql -U app -d exports -c "SELECT * FROM river_job;"  # rescued jobs go running -> available -> completed
```

If you see `fatal error: newosproc` in worker logs, raise `pids_limit` (pids count threads too; Go + River + x264 easily exceeds 128).

## Duplicate or missing chunk in final video

Likely cause: concat order wrong, planner off-by-one (gap/overlap), or the `UNIQUE(export_id,idx)` was bypassed.

```powershell
docker compose exec -T postgres psql -U app -d exports -c "SELECT idx,start_ms,end_ms,status,attempts FROM export_chunks WHERE export_id='<id>' ORDER BY idx;"
# must be contiguous: each start_ms == previous end_ms, first starts at 0, last ends at asset duration_ms
docker compose exec -T postgres psql -U app -d exports -c "SELECT idx,count(*) FROM export_chunks WHERE export_id='<id>' GROUP BY idx HAVING count(*)>1;"
# ^ must return 0 rows (unique constraint). exactly one completed stitch job per export:
docker compose exec -T postgres psql -U app -d exports -c "SELECT count(*) FROM river_job WHERE kind='export-stitch' AND state='completed' AND args->>'ExportID'='<id>';"
```

Fix: planner builds cut points then derives bounds (`internal/media/media.go: PlanChunks`), so contiguity is structural. Stitch builds the concat list `ORDER BY idx`. Re-running a done chunk is a no-op (`CompleteChunk` returns success when already `done`).

## A/V drift or glitch at chunk joins

Likely cause: cut on non-keyframe, inconsistent encode settings per chunk, or audio priming differences.

```powershell
# keyframes of source (cuts should snap near these):
docker compose exec -T api ffprobe -v error -select_streams v:0 -skip_frame nokey -show_entries frame=pkt_pts_time -of csv=p=0 /data/media/originals/<asset>.mp4
# per-chunk durations:
docker compose exec -T api ffprobe -v error -show_entries format=duration -of csv=p=0 /data/media/chunks/<export>/chunk-*.mp4
```

Fix: planner snaps boundaries to nearest keyframe within half a chunk (`PlanChunks`); every chunk uses identical fixed settings (`-g 48 -keyint_min 48 -sc_threshold 0`, same audio `aac 128k/48k/stereo`); stitch uses concat demuxer with `-c copy` (no re-encode). ~20ms overall duration delta from AAC priming is expected.

## Frame count mismatch

Likely cause: segment boundary rounding, `-vsync` drops/dups, or comparing against the wrong file.

```powershell
docker compose exec -T api ffprobe -v error -select_streams v:0 -count_frames -show_entries stream=nb_read_frames -of csv=p=0 /data/media/originals/<asset>.mp4
docker compose exec -T api ffprobe -v error -select_streams v:0 -count_frames -show_entries stream=nb_read_frames -of csv=p=0 /data/media/exports/<export>/output.mp4
docker compose exec -T postgres psql -U app -d exports -c "SELECT idx,start_ms,end_ms FROM export_chunks WHERE export_id='<id>' ORDER BY idx;"
```

Fix: chunks use `-vsync cfr` and cover `[0,duration)` contiguously; stitch is a stream copy so frames are preserved bit-exact.

## Kill-run hash != clean-run hash

Likely cause: non-determinism (threads, x264 lookahead/auto params, different ffmpeg versions).

```powershell
docker compose exec -T api ffmpeg -version | head -n 1   # must be identical on all workers (same image)
docker compose exec -T api sha256sum /data/media/exports/<kill-run>/output.mp4 /data/media/exports/<clean-run>/output.mp4
```

Fix: determinism is structural here: `-threads 1` per chunk, fixed GOP (`-g 48 -keyint_min 48 -sc_threshold 0`), `-fflags +bitexact -flags:v +bitexact`, `-map_metadata -1`, same pinned ffmpeg in one Docker image for api+worker. If hashes still differ, check the ffmpeg version string first.

## ffmpeg exits with signal / OOM (exit code 137)

Likely cause: container memory limit; 1080p x264 needs headroom per concurrent encode.

```powershell
docker stats --no-stream
docker compose logs worker | Select-String "exit status 137|signal killed|Cannot allocate memory|Error while opening encoder"
docker inspect pipe-worker-1 --format '{{.State.OOMKilled}}'
```

Fix: raise worker `mem_limit` (currently 4g) and/or lower `RIVER_MAX_WORKERS` (currently 4) so fewer encodes run concurrently. Transient `Error while opening encoder` under CPU/memory pressure is retryable and converges (see worker logs `attempt=N` then `finished`).

## Jobs not picked up (stuck in planned/rendering, no worker logs)

Likely cause: River migrations not run, wrong queue name, or no workers polling.

```powershell
docker compose exec -T postgres psql -U app -d exports -c "SELECT count(*) FROM river_migration;"
docker compose exec -T postgres psql -U app -d exports -c "SELECT kind,state,count(*) FROM river_job GROUP BY 1,2 ORDER BY 1,2;"
docker compose logs -f worker
docker compose ps
```

Fix: the binary runs River migrations at boot (`rivermigrate` in `cmd/server/main.go`); all jobs use the `default` queue and workers subscribe to it. After `docker kill`, rescue takes ~`RESCUE_STUCK_AFTER_S` + 30s tick — wait, don't re-enqueue manually (that would duplicate chunk jobs; chunk claim is conditional so duplicates are harmless but noisy).

## SSE stalls behind a proxy

Likely cause: response buffering (reverse proxy / `curl` without `-N`).

```powershell
curl.exe -N -H "X-API-Key: <key>" http://localhost:8080/exports/<id>/events
# resume from a dropped connection:
curl.exe -N -H "X-API-Key: <key>" -H "Last-Event-ID: 12" http://localhost:8080/exports/<id>/events
```

Fix: handler sets `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `X-Accel-Buffering: no` and flushes after every event (`internal/api/server.go: handleEvents`). Resume works via `Last-Event-ID`: the server replays current per-chunk state from the DB (source of truth) then streams deltas.

## General inspection cheat sheet

```powershell
docker compose logs -f worker
docker compose logs -f api
docker compose exec -T postgres psql -U app -d exports -c "SELECT id,status,chunk_count,error FROM exports ORDER BY created_at DESC LIMIT 5;"
docker compose exec -T postgres psql -U app -d exports -c "SELECT export_id,idx,status,attempts,worker_id FROM export_chunks ORDER BY export_id,idx;"
docker compose exec -T postgres psql -U app -d exports -c "SELECT kind,state,attempt,left(errors::text,200) FROM river_job ORDER BY id DESC LIMIT 10;"
docker compose exec -T api ffprobe -v error -show_streams /data/media/originals/<asset>.mp4
docker compose exec -T api ffprobe -v error -select_streams v:0 -skip_frame nokey -show_entries frame=pkt_pts_time -of csv=p=0 /data/media/originals/<asset>.mp4
docker kill <worker-container>   # chaos: compose restarts it (restart: unless-stopped)
```
