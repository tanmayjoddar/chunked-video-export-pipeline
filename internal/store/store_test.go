package store

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration: needs DATABASE_URL (real Postgres). Skipped otherwise.
func TestConditionalUpdates(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	s := New(pool)
	aid, err := s.CreateAsset(ctx, "/tmp/x.mp4", 10)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := s.TransitionAsset(ctx, aid, "uploaded", "validated")
	if err != nil || !ok {
		t.Fatalf("first transition should win: %v %v", ok, err)
	}
	ok2, err := s.TransitionAsset(ctx, aid, "uploaded", "validated")
	if err != nil {
		t.Fatal(err)
	}
	if ok2 {
		t.Fatal("stale transition must NOT overwrite (expect false)")
	}
	eid, err := s.CreateExport(ctx, aid)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertChunks(ctx, eid, [][2]int64{{0, 1000}, {1000, 2000}}); err != nil {
		t.Fatal(err)
	}
	ok, _ = s.ClaimChunk(ctx, eid, 0, "w1")
	if !ok {
		t.Fatal("claim should win")
	}
	done, err := s.CompleteChunk(ctx, eid, 0, "/tmp/c0.mp4", "abc")
	if err != nil || !done {
		t.Fatalf("complete: %v %v", done, err)
	}
	// re-run of done chunk is a no-op success
	done2, err := s.CompleteChunk(ctx, eid, 0, "/tmp/c0.mp4", "abc")
	if err != nil || !done2 {
		t.Fatalf("idempotent re-complete failed: %v %v", done2, err)
	}
	// stale claim after done must fail
	ok, _ = s.ClaimChunk(ctx, eid, 0, "w2")
	if ok {
		t.Fatal("claim on done chunk must fail")
	}
}
