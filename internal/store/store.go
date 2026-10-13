package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	Pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{Pool: pool} }

// ---- allowed transitions (documented contract) ----
// assets: uploaded -> validated | rejected | failed
//         validated -> proxy_ready | failed
// exports: planned -> rendering -> stitching -> done
//          planned|rendering|stitching -> failed | canceled
// chunks: pending -> running -> done | failed
//         running -> pending (release on worker death; attempts++)
//         failed -> pending (retry; attempts++)
// All writes below are conditional UPDATEs (WHERE status=...).

type Asset struct {
	ID           string
	Status       string
	OriginalPath string
	ProxyPath    *string
	DurationMs   *int64
	Width        *int
	Height       *int
	Codec        *string
	SizeBytes    *int64
}

type Export struct {
	ID         string
	AssetID    string
	Status     string
	ChunkCount int
	OutputPath *string
	Error      *string
}

type Chunk struct {
	ID          string
	ExportID    string
	Idx         int
	StartMs     int64
	EndMs       int64
	Status      string
	Attempts    int
	WorkerID    *string
	OutputPath  *string
	Checksum    *string
	HeartbeatAt *string
}

func (s *Store) CreateAsset(ctx context.Context, originalPath string, sizeBytes int64) (string, error) {
	return s.CreateAssetWithID(ctx, uuid.NewString(), originalPath, sizeBytes)
}

func (s *Store) CreateAssetWithID(ctx context.Context, id, originalPath string, sizeBytes int64) (string, error) {
	_, err := s.Pool.Exec(ctx, `INSERT INTO assets(id,status,original_path,size_bytes) VALUES($1,'uploaded',$2,$3)`, id, originalPath, sizeBytes)
	if err != nil {
		return "", fmt.Errorf("create asset: %w", err)
	}
	return id, nil
}

func (s *Store) GetAsset(ctx context.Context, id string) (*Asset, error) {
	a := &Asset{}
	err := s.Pool.QueryRow(ctx, `SELECT id,status,original_path,proxy_path,duration_ms,width,height,codec,size_bytes FROM assets WHERE id=$1`, id).
		Scan(&a.ID, &a.Status, &a.OriginalPath, &a.ProxyPath, &a.DurationMs, &a.Width, &a.Height, &a.Codec, &a.SizeBytes)
	if err != nil {
		return nil, fmt.Errorf("get asset: %w", err)
	}
	return a, nil
}

// Conditional asset transition. Returns true if row was updated.
func (s *Store) TransitionAsset(ctx context.Context, id, from, to string) (bool, error) {
	ct, err := s.Pool.Exec(ctx, `UPDATE assets SET status=$3 WHERE id=$1 AND status=$2`, id, from, to)
	if err != nil {
		return false, fmt.Errorf("transition asset: %w", err)
	}
	return ct.RowsAffected() == 1, nil
}

func (s *Store) MarkAssetValidated(ctx context.Context, id string, durMs int64, w, h int, codec string) error {
	ct, err := s.Pool.Exec(ctx, `UPDATE assets SET status='validated',duration_ms=$2,width=$3,height=$4,codec=$5 WHERE id=$1 AND status='uploaded'`, id, durMs, w, h, codec)
	if err != nil {
		return fmt.Errorf("mark validated: %w", err)
	}
	if ct.RowsAffected() != 1 {
		return fmt.Errorf("mark validated: conditional update failed (stale state)")
	}
	return nil
}

func (s *Store) MarkAssetRejected(ctx context.Context, id, reason string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE assets SET status='rejected', reject_reason=$2 WHERE id=$1 AND status='uploaded'`, id, reason)
	return err
}

func (s *Store) MarkProxyReady(ctx context.Context, id, proxyPath string) error {
	ct, err := s.Pool.Exec(ctx, `UPDATE assets SET status='proxy_ready', proxy_path=$2 WHERE id=$1 AND status='validated'`, id, proxyPath)
	if err != nil {
		return fmt.Errorf("mark proxy_ready: %w", err)
	}
	if ct.RowsAffected() != 1 {
		// idempotent: already proxy_ready with same path is fine
		var st string
		_ = s.Pool.QueryRow(ctx, `SELECT status FROM assets WHERE id=$1`, id).Scan(&st)
		if st == "proxy_ready" {
			return nil
		}
		return fmt.Errorf("mark proxy_ready: conditional update failed")
	}
	return nil
}

func (s *Store) CreateExport(ctx context.Context, assetID string) (string, error) {
	id := uuid.NewString()
	_, err := s.Pool.Exec(ctx, `INSERT INTO exports(id,asset_id,status,chunk_count) VALUES($1,$2,'planned',0)`, id, assetID)
	if err != nil {
		return "", fmt.Errorf("create export: %w", err)
	}
	return id, nil
}

func (s *Store) GetExport(ctx context.Context, id string) (*Export, error) {
	e := &Export{}
	err := s.Pool.QueryRow(ctx, `SELECT id,asset_id,status,chunk_count,output_path,error FROM exports WHERE id=$1`, id).
		Scan(&e.ID, &e.AssetID, &e.Status, &e.ChunkCount, &e.OutputPath, &e.Error)
	if err != nil {
		return nil, fmt.Errorf("get export: %w", err)
	}
	return e, nil
}

func (s *Store) TransitionExport(ctx context.Context, id, from, to string) (bool, error) {
	ct, err := s.Pool.Exec(ctx, `UPDATE exports SET status=$3 WHERE id=$1 AND status=$2`, id, from, to)
	if err != nil {
		return false, fmt.Errorf("transition export: %w", err)
	}
	return ct.RowsAffected() == 1, nil
}

func (s *Store) SetExportChunks(ctx context.Context, id string, n int, from, to string) (bool, error) {
	ct, err := s.Pool.Exec(ctx, `UPDATE exports SET status=$4, chunk_count=$3 WHERE id=$1 AND status=$2`, id, from, n, to)
	if err != nil {
		return false, fmt.Errorf("set chunks: %w", err)
	}
	return ct.RowsAffected() == 1, nil
}

func (s *Store) FinishExport(ctx context.Context, id, outputPath, from string) error {
	ct, err := s.Pool.Exec(ctx, `UPDATE exports SET status='done', output_path=$2, finished_at=now() WHERE id=$1 AND status=$3`, id, outputPath, from)
	if err != nil {
		return fmt.Errorf("finish export: %w", err)
	}
	if ct.RowsAffected() != 1 {
		return fmt.Errorf("finish export: conditional update failed")
	}
	return nil
}

func (s *Store) FailExport(ctx context.Context, id, msg string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE exports SET status='failed', error=$2, finished_at=now() WHERE id=$1 AND status IN ('planned','rendering','stitching')`, id, msg)
	return err
}

func (s *Store) InsertChunks(ctx context.Context, exportID string, bounds [][2]int64) error {
	for i, b := range bounds {
		_, err := s.Pool.Exec(ctx, `INSERT INTO export_chunks(export_id,idx,start_ms,end_ms,status) VALUES($1,$2,$3,$4,'pending') ON CONFLICT (export_id,idx) DO NOTHING`, exportID, i, b[0], b[1])
		if err != nil {
			return fmt.Errorf("insert chunk %d: %w", i, err)
		}
	}
	return nil
}

func (s *Store) ListChunks(ctx context.Context, exportID string) ([]Chunk, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,export_id,idx,start_ms,end_ms,status,attempts,worker_id,output_path,checksum FROM export_chunks WHERE export_id=$1 ORDER BY idx`, exportID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Chunk{}
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.ID, &c.ExportID, &c.Idx, &c.StartMs, &c.EndMs, &c.Status, &c.Attempts, &c.WorkerID, &c.OutputPath, &c.Checksum); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ClaimChunk: pending|failed -> running (conditional), attempts++.
func (s *Store) ClaimChunk(ctx context.Context, exportID string, idx int, workerID string) (bool, error) {
	ct, err := s.Pool.Exec(ctx, `UPDATE export_chunks SET status='running', attempts=attempts+1, worker_id=$3, started_at=now(), heartbeat_at=now(), error=NULL WHERE export_id=$1 AND idx=$2 AND status IN ('pending','failed')`, exportID, idx, workerID)
	if err != nil {
		return false, fmt.Errorf("claim chunk: %w", err)
	}
	return ct.RowsAffected() == 1, nil
}

// CompleteChunk: running -> done (conditional). Idempotent: if already done, return true,nil.
func (s *Store) CompleteChunk(ctx context.Context, exportID string, idx int, outputPath, checksum string) (bool, error) {
	ct, err := s.Pool.Exec(ctx, `UPDATE export_chunks SET status='done', output_path=$3, checksum=$4, finished_at=now() WHERE export_id=$1 AND idx=$2 AND status='running'`, exportID, idx, outputPath, checksum)
	if err != nil {
		return false, fmt.Errorf("complete chunk: %w", err)
	}
	if ct.RowsAffected() == 1 {
		return true, nil
	}
	var st string
	if err := s.Pool.QueryRow(ctx, `SELECT status FROM export_chunks WHERE export_id=$1 AND idx=$2`, exportID, idx).Scan(&st); err != nil {
		return false, err
	}
	if st == "done" {
		return true, nil // re-run of a done chunk is a no-op
	}
	return false, nil
}

func (s *Store) FailChunk(ctx context.Context, exportID string, idx int, msg string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE export_chunks SET status='failed', error=$3 WHERE export_id=$1 AND idx=$2 AND status='running'`, exportID, idx, msg)
	return err
}

func (s *Store) HeartbeatChunk(ctx context.Context, exportID string, idx int) error {
	_, err := s.Pool.Exec(ctx, `UPDATE export_chunks SET heartbeat_at=now() WHERE export_id=$1 AND idx=$2 AND status='running'`, exportID, idx)
	return err
}

// ReleaseStuckChunks: running with old heartbeat -> pending (for rescue watchdog).
func (s *Store) ReleaseStuckChunks(ctx context.Context, olderThanS int) (int64, error) {
	ct, err := s.Pool.Exec(ctx, `UPDATE export_chunks SET status='pending', worker_id=NULL WHERE status='running' AND heartbeat_at < now() - make_interval(secs=>$1)`, float64(olderThanS))
	if err != nil {
		return 0, err
	}
	return ct.RowsAffected(), nil
}

func (s *Store) CountPending(ctx context.Context, exportID string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM export_chunks WHERE export_id=$1 AND status IN ('pending','running','failed')`, exportID).Scan(&n)
	return n, err
}

func (s *Store) CountDone(ctx context.Context, exportID string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM export_chunks WHERE export_id=$1 AND status='done'`, exportID).Scan(&n)
	return n, err
}
