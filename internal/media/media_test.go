package media

import "testing"

func TestPlanChunks(t *testing.T) {
	kf := []float64{0, 5, 10, 15, 20, 25, 30}
	cases := []struct {
		name     string
		dur      int64
		chunkSec int
		max      int
		wantN    int
		wantErr  bool
	}{
		{"30s/10s -> 3", 30000, 10, 64, 3, false},
		{"5s/10s -> 1", 5000, 10, 64, 1, false},
		{"61s/10s -> 7ish", 61000, 10, 64, 7, false},
		{"over max", 1000000, 1, 4, 0, true},
		{"zero dur", 0, 10, 64, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := PlanChunks(c.dur, kf, c.chunkSec, c.max)
			if c.wantErr {
				if err == nil {
					t.Fatal("want err")
				}
				return
			}
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if len(got) != c.wantN {
				t.Fatalf("got %d chunks want %d (%v)", len(got), c.wantN, got)
			}
			// contiguity + coverage
			if got[0][0] != 0 || got[len(got)-1][1] != c.dur {
				t.Fatalf("bounds don't cover [0,%d): %v", c.dur, got)
			}
			for i := 1; i < len(got); i++ {
				if got[i][0] != got[i-1][1] {
					t.Fatalf("gap/overlap at %d: %v", i, got)
				}
			}
		})
	}
}

func TestPlanChunksSnapsToKeyframe(t *testing.T) {
	kf := []float64{0, 10.0, 20.04, 30.0}
	got, err := PlanChunks(30000, kf, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 got %v", got)
	}
	// last boundary should snap near 20.04s keyframe
	if got[2][0] < 19500 || got[2][0] > 20600 {
		t.Fatalf("expected snap near 20040, got %v", got)
	}
}
