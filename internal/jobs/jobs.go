package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/example/chunked-export/internal/config"
	"github.com/example/chunked-export/internal/media"
	"github.com/example/chunked-export/internal/store"
)

// River queue names. Docs: riverqueue.com/docs (queues), pkg.go.dev/github.com/riverqueue/river.
const (
	QueueDefault = river.QueueDefault // "default"
)

// ---- job args (Kind = job type string) ----

type ProxyArgs struct{ AssetID string }

func (ProxyArgs) Kind() string { return "proxy" }

type PlanArgs struct {
	ExportID string
	AssetID  string
}

func (PlanArgs) Kind() string { return "export-plan" }

type ChunkArgs struct {
	ExportID string
	Idx      int
}

func (ChunkArgs) Kind() string { return "export-chunk" }

type StitchArgs struct{ ExportID string }

func (StitchArgs) Kind() string { return "export-stitch" }

// ---- deps shared by workers ----

type Deps struct {
	Config *config.Config
	Store  *store.Store
	Client *river.Client[pgx.Tx]
}

func workerID() string { return uuid.NewString()[:8] }

// ---- proxy worker: 720p H.264, tmp+rename, idempotent ----

type ProxyWorker struct {
	river.WorkerDefaults[ProxyArgs]
	Deps *Deps
}

func (w *ProxyWorker) Work(ctx context.Context, job *river.Job[ProxyArgs]) error {
	log := slog.With("job", "proxy", "asset_id", job.Args.AssetID, "attempt", job.Attempt)
	log.Info("queued")
	a, err := w.Deps.Store.GetAsset(ctx, job.Args.AssetID)
	if err != nil {
		return river.JobCancel(fmt.Errorf("asset missing: %w", err))
	}
	if a.Status == "proxy_ready" {
		log.Info("finished (already done, no-op)")
		return nil
	}
	// validate session: asset must be validated
	if a.Status != "validated" {
		// proxy job enqueued right after validation; if not validated yet, snooze briefly
		return river.JobSnooze(5 * time.Second)
	}
	dst := filepath.Join(w.Deps.Config.StorageDir, "proxy", job.Args.AssetID+".mp4")
	if err := media.RunToTemp(ctx, w.Deps.Config.FFmpegBin, media.ProxyArgs(a.OriginalPath, dst), dst, w.Deps.Config.JobTimeoutS); err != nil {
		return fmt.Errorf("proxy ffmpeg (retryable): %w", err)
	}
	if err := w.Deps.Store.MarkProxyReady(ctx, job.Args.AssetID, dst); err != nil {
		return fmt.Errorf("mark proxy_ready: %w", err)
	}
	log.Info("finished", "proxy_path", "proxy/<id>.mp4")
	return nil
}

// Timeout returns per-job deadline.
func (w *ProxyWorker) Timeout(job *river.Job[ProxyArgs]) time.Duration { return 10 * time.Minute }

// ---- plan worker: keyframe split + enqueue chunk jobs ----

type PlanWorker struct {
	river.WorkerDefaults[PlanArgs]
	Deps *Deps
}

func (w *PlanWorker) Work(ctx context.Context, job *river.Job[PlanArgs]) error {
	log := slog.With("job", "plan", "export_id", job.Args.ExportID, "attempt", job.Attempt)
	log.Info("started")
	exp, err := w.Deps.Store.GetExport(ctx, job.Args.ExportID)
	if err != nil {
		return river.JobCancel(fmt.Errorf("export missing: %w", err))
	}
	if exp.Status != "planned" {
		log.Info("finished (already planned, no-op)")
		return nil
	}
	a, err := w.Deps.Store.GetAsset(ctx, job.Args.AssetID)
	if err != nil {
		return river.JobCancel(fmt.Errorf("asset missing: %w", err))
	}
	if a.DurationMs == nil {
		return river.JobCancel(fmt.Errorf("asset has no duration"))
	}
	kf, err := media.KeyframeTimes(ctx, w.Deps.Config.FFprobeBin, a.OriginalPath)
	if err != nil {
		return fmt.Errorf("keyframe probe (retryable): %w", err)
	}
	bounds, err := media.PlanChunks(*a.DurationMs, kf, w.Deps.Config.ChunkSeconds, w.Deps.Config.MaxChunks)
	if err != nil {
		_ = w.Deps.Store.FailExport(ctx, job.Args.ExportID, err.Error())
		return river.JobCancel(err)
	}
	if err := w.Deps.Store.InsertChunks(ctx, job.Args.ExportID, bounds); err != nil {
		return fmt.Errorf("insert chunks: %w", err)
	}
	ok, err := w.Deps.Store.SetExportChunks(ctx, job.Args.ExportID, len(bounds), "planned", "rendering")
	if err != nil || !ok {
		return fmt.Errorf("transition planned->rendering: %w", err)
	}
	// enqueue one chunk job per chunk (idempotent: unique per export+idx via UniqueOpts? use plain insert; re-plan is no-op due to status gate)
	for i := range bounds {
		if _, err := w.Deps.Client.Insert(ctx, ChunkArgs{ExportID: job.Args.ExportID, Idx: i}, &river.InsertOpts{
			Queue:       QueueDefault,
			MaxAttempts: 10,
		}); err != nil {
			return fmt.Errorf("enqueue chunk %d: %w", i, err)
		}
		log.Info("chunk queued", "idx", i)
	}
	log.Info("finished", "chunks", len(bounds))
	return nil
}

func (w *PlanWorker) Timeout(job *river.Job[PlanArgs]) time.Duration { return 5 * time.Minute }

// ---- chunk worker ----

type ChunkWorker struct {
	river.WorkerDefaults[ChunkArgs]
	Deps *Deps
}

func (w *ChunkWorker) Work(ctx context.Context, job *river.Job[ChunkArgs]) error {
	wid := workerID()
	log := slog.With("job", "chunk", "export_id", job.Args.ExportID, "idx", job.Args.Idx, "attempt", job.Attempt, "worker", wid)
	exp, err := w.Deps.Store.GetExport(ctx, job.Args.ExportID)
	if err != nil {
		return river.JobCancel(fmt.Errorf("export missing: %w", err))
	}
	if exp.Status == "done" || exp.Status == "canceled" {
		return nil
	}
	chunks, err := w.Deps.Store.ListChunks(ctx, job.Args.ExportID)
	if err != nil {
		return fmt.Errorf("list chunks: %w", err)
	}
	var mine *store.Chunk
	for i := range chunks {
		if chunks[i].Idx == job.Args.Idx {
			mine = &chunks[i]
			break
		}
	}
	if mine == nil {
		return river.JobCancel(fmt.Errorf("chunk %d not found", job.Args.Idx))
	}
	if mine.Status == "done" {
		log.Info("finished (already done, no-op)")
		return nil // idempotent re-run
	}
	ok, err := w.Deps.Store.ClaimChunk(ctx, job.Args.ExportID, job.Args.Idx, wid)
	if err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	if !ok {
		// another worker owns it (stale retry) — do not overwrite
		log.Info("skip: already claimed by another worker")
		return nil
	}
	log.Info("started")
	// heartbeat loop
	hbCtx, hbCancel := context.WithCancel(context.Background())
	defer hbCancel()
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				_ = w.Deps.Store.HeartbeatChunk(context.Background(), job.Args.ExportID, job.Args.Idx)
			}
		}
	}()

	a, err := w.Deps.Store.GetAsset(ctx, exp.AssetID)
	if err != nil {
		_ = w.Deps.Store.FailChunk(ctx, job.Args.ExportID, job.Args.Idx, "asset missing")
		return river.JobCancel(err)
	}
	dst := filepath.Join(w.Deps.Config.StorageDir, "chunks", job.Args.ExportID, fmt.Sprintf("chunk-%04d.mp4", job.Args.Idx))
	// idempotent: if chunk file already done in DB we returned; if file exists but DB running, re-render to tmp+rename (same deterministic bytes)
	if err := media.RunToTemp(ctx, w.Deps.Config.FFmpegBin,
		media.ChunkArgs(a.OriginalPath, dst, mine.StartMs, mine.EndMs), dst, w.Deps.Config.JobTimeoutS); err != nil {
		_ = w.Deps.Store.FailChunk(ctx, job.Args.ExportID, job.Args.Idx, truncateErr(err))
		return fmt.Errorf("chunk ffmpeg (retryable): %w", err)
	}
	sum, err := media.SHA256Stream(dst)
	if err != nil {
		_ = w.Deps.Store.FailChunk(ctx, job.Args.ExportID, job.Args.Idx, "checksum failed")
		return fmt.Errorf("checksum: %w", err)
	}
	done, err := w.Deps.Store.CompleteChunk(ctx, job.Args.ExportID, job.Args.Idx, dst, sum)
	if err != nil || !done {
		return fmt.Errorf("complete chunk: %w", err)
	}
	// verify output looks sane (non-empty)
	if fi, err := os.Stat(dst); err != nil || fi.Size() == 0 {
		_ = w.Deps.Store.FailChunk(ctx, job.Args.ExportID, job.Args.Idx, "empty output")
		return fmt.Errorf("empty chunk output")
	}
	log.Info("finished")
	// if all chunks done, enqueue stitch (unique: only one stitcher wins the rendering->stitching transition)
	rem, err := w.Deps.Store.CountPending(ctx, job.Args.ExportID)
	if err != nil {
		return fmt.Errorf("count pending: %w", err)
	}
	if rem == 0 {
		ok, err := w.Deps.Store.TransitionExport(ctx, job.Args.ExportID, "rendering", "stitching")
		if err != nil {
			return fmt.Errorf("to stitching: %w", err)
		}
		if ok {
			if _, err := w.Deps.Client.Insert(ctx, StitchArgs{ExportID: job.Args.ExportID}, &river.InsertOpts{Queue: QueueDefault, MaxAttempts: 5}); err != nil {
				return fmt.Errorf("enqueue stitch: %w", err)
			}
			slog.With("export_id", job.Args.ExportID).Info("stitch queued")
		}
	}
	return nil
}

func (w *ChunkWorker) Timeout(job *river.Job[ChunkArgs]) time.Duration { return 15 * time.Minute }

// ---- stitch worker: concat demuxer -c copy ----

type StitchWorker struct {
	river.WorkerDefaults[StitchArgs]
	Deps *Deps
}

func (w *StitchWorker) Work(ctx context.Context, job *river.Job[StitchArgs]) error {
	log := slog.With("job", "stitch", "export_id", job.Args.ExportID, "attempt", job.Attempt)
	log.Info("started")
	exp, err := w.Deps.Store.GetExport(ctx, job.Args.ExportID)
	if err != nil {
		return river.JobCancel(fmt.Errorf("export missing: %w", err))
	}
	if exp.Status == "done" {
		log.Info("finished (already done, no-op)")
		return nil
	}
	if exp.Status != "stitching" {
		return river.JobCancel(fmt.Errorf("export status %q, expected stitching", exp.Status))
	}
	chunks, err := w.Deps.Store.ListChunks(ctx, job.Args.ExportID)
	if err != nil {
		return fmt.Errorf("list chunks: %w", err)
	}
	for _, c := range chunks {
		if c.Status != "done" {
			// not all done yet (stale trigger): release back to rendering
			_, _ = w.Deps.Store.TransitionExport(ctx, job.Args.ExportID, "stitching", "rendering")
			return river.JobSnooze(5 * time.Second)
		}
	}
	// build concat list with server-generated UUID paths only (escaped)
	listPath := filepath.Join(w.Deps.Config.StorageDir, "exports", job.Args.ExportID, "list.txt")
	if err := os.MkdirAll(filepath.Dir(listPath), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	var sb string
	for _, c := range chunks {
		if c.OutputPath == nil {
			return river.JobCancel(fmt.Errorf("chunk %d has no output", c.Idx))
		}
		// escape single quotes for concat demuxer
		p := *c.OutputPath
		p = replaceQuote(p)
		sb += "file '" + p + "'\n"
	}
	if err := os.WriteFile(listPath, []byte(sb), 0o644); err != nil {
		return fmt.Errorf("write list: %w", err)
	}
	dst := filepath.Join(w.Deps.Config.StorageDir, "exports", job.Args.ExportID, "output.mp4")
	if err := media.RunConcat(ctx, w.Deps.Config.FFmpegBin, listPath, dst, w.Deps.Config.JobTimeoutS); err != nil {
		return fmt.Errorf("concat (retryable): %w", err)
	}
	if err := w.Deps.Store.FinishExport(ctx, job.Args.ExportID, dst, "stitching"); err != nil {
		return fmt.Errorf("finish export: %w", err)
	}
	log.Info("finished")
	return nil
}

func (w *StitchWorker) Timeout(job *river.Job[StitchArgs]) time.Duration { return 10 * time.Minute }

func replaceQuote(s string) string {
	out := ""
	for _, r := range s {
		if r == '\'' {
			out += "'\\''"
		} else {
			out += string(r)
		}
	}
	return out
}

func truncateErr(err error) string {
	s := err.Error()
	if len(s) > 500 {
		return s[:500]
	}
	return s
}
