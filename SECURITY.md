# SECURITY.md

Every uploaded file is treated as hostile. Below is the threat model:
threat -> mitigation -> where in code.

## Threat model

| Threat | Mitigation | Where |
|---|---|---|
| Shell injection via filenames/params | No shell, ever. `exec.CommandContext(ctx, "ffmpeg", args...)` with args slices only | `internal/media/media.go` (`run`, `ProxyArgs`, `ChunkArgs`, `ConcatArgs`) |
| Path traversal via client filename | Storage paths are server-generated UUIDs under fixed base dir; client filename kept as metadata only; `filepath.Clean` + prefix check before any use | `internal/api/server.go` (`watchUploads`, `handleDownload`) |
| Malicious container/codec (fuzzed MP4, exotic codecs as exploit carriers) | Validate by content: magic bytes first, then `ffprobe` JSON; allowlist containers (`mp4/mov/mkv-webm family`) and video codecs (`h264/hevc/prores/vp9`); anything else -> asset `rejected` with reason | `internal/media/media.go` (`MagicBytes`, `ProbeFile`, `ValidateProbe`) |
| SSRF via media playlists (HLS/concat referencing http URLs) | `-protocol_whitelist file,pipe` on every ffmpeg/ffprobe invocation | `internal/media/media.go` (`baseArgs`, all probe calls) |
| ffmpeg hanging on crafted input | Per-job `context.WithTimeout` (`JOB_TIMEOUT_S`), `-nostdin` on ffmpeg (never attach server stdin) | `internal/media/media.go` (`RunToTemp`), `internal/jobs/jobs.go` (`Timeout` methods) |
| Oversized upload (disk fill) | `MaxSize` on tusd handler + size check in hook + `MAX_UPLOAD_BYTES` | `internal/api/server.go` (tusd `Config.MaxSize`), `internal/config/config.go` |
| Oversized job (duration/resolution/stream bomb) | `MAX_DURATION_MS`, `MAX_WIDTH/HEIGHT`, `MAX_STREAMS`, `MAX_CHUNKS` enforced in ffprobe validation and planner | `internal/media/media.go` (`ValidateProbe`, `PlanChunks`), `internal/config/config.go` |
| Malicious concat list (path injection into stitch) | List file generated server-side from UUID paths only, single-quote escaping; `-safe 0` with allowlisted dir | `internal/jobs/jobs.go` (`StitchWorker`) |
| Static serving of storage dir | Never served statically; downloads only via authenticated `/exports/{id}/download` with sanitized `Content-Disposition` | `internal/api/server.go` (`handleDownload`) |
| Unauthenticated API/tus/SSE access | API key from `.env`, `subtle.ConstantTimeCompare`; required on `/exports`, `/assets`, `/files`, SSE (`?api_key=` fallback for EventSource) | `internal/api/server.go` (`checkKey`, `wrap`) |
| API abuse (huge JSON, slowloris, CORS wildcard, flooding) | `http.MaxBytesReader` 1MB JSON cap, server timeouts (ReadHeader/Read/Write/Idle), strict CORS allowlist from `.env`, per-key rate limiter, `nosniff` + `no-store` headers | `internal/api/server.go` (`wrap`), `cmd/server/main.go` (`http.Server`) |
| SQL injection | `pgx` parameterized queries only (`$1...`), no string-built SQL | `internal/store/store.go` |
| Over-privileged DB role | App uses least-privilege role (see `docker-compose.yml` / README: `app` role, no superuser in DSN) | `docker-compose.yml` |
| Secret leak (keys in repo/logs) | `.env` gitignored, `.env.example` placeholders only, startup fails on missing/weak key, logs never print keys or client paths | `internal/config/config.go` (`Load`), `.gitignore` |
| Container escape / lateral movement | Non-root user (`appuser:10001`), `read_only: true` rootfs + `tmpfs /tmp` + one writable volume, `cap_drop: ALL`, `no-new-privileges`, memory/cpu/pids limits; worker has no inbound ports | `Dockerfile`, `docker-compose.yml` |
| Stale worker overwriting newer state | All status changes are conditional `UPDATE ... WHERE status = expected`; `UNIQUE(export_id, idx)` | `internal/store/store.go`, `migrations/001_init.sql` |
| Non-reproducible builds / decoder CVEs | Pinned base images + pinned ffmpeg (`debian:bookworm-slim`, `ffmpeg=7:5.1.*`); ffmpeg version printed at build | `Dockerfile` |
| Duplicate side effects on retry (at-least-once queue) | Idempotent jobs: write `<name>.tmp.mp4` then `os.Rename`; re-running a done chunk is a no-op | `internal/media/media.go` (`RunToTemp`), `internal/jobs/jobs.go` |

## Known limits (honest)

- No virus/malware scanning of uploads (single-tenant portfolio project; add ClamAV sidecar before multi-tenant use).
- API-key auth only: no users, roles, or key rotation. One key in `.env`.
- Single-tenant: no per-user isolation of assets/exports.
- No TLS termination in compose (put a reverse proxy in front in production).
- Rate limiter is in-memory per api replica (not shared).
