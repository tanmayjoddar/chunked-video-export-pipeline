package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/example/chunked-export/internal/config"
	"github.com/example/chunked-export/internal/jobs"
	"github.com/example/chunked-export/internal/media"
	"github.com/example/chunked-export/internal/store"

	"github.com/tus/tusd/v2/pkg/filelocker"
	"github.com/tus/tusd/v2/pkg/filestore"
	tusd "github.com/tus/tusd/v2/pkg/handler"
)

type Server struct {
	Cfg    *config.Config
	Store  *store.Store
	Client *river.Client[pgx.Tx]
	Mux    *http.ServeMux
	rl     *rateLimiter
}

type APIError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

func writeErr(w http.ResponseWriter, reqID string, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(APIError{Code: code, Message: msg, RequestID: reqID})
}

func New(cfg *config.Config, st *store.Store, client *river.Client[pgx.Tx]) (*Server, error) {
	s := &Server{Cfg: cfg, Store: st, Client: client, Mux: http.NewServeMux(), rl: newRateLimiter(cfg.RateLimitRPS)}
	// tusd embedded handler at /files/. Docs: tus.github.io/tusd/advanced-topics/usage-package/
	if err := os.MkdirAll(cfg.TusdDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.StorageDir, 0o755); err != nil {
		return nil, err
	}
	fstore := filestore.New(cfg.TusdDir)
	locker := filelocker.New(cfg.TusdDir)
	composer := tusd.NewStoreComposer()
	fstore.UseIn(composer)
	locker.UseIn(composer)
	handler, err := tusd.NewHandler(tusd.Config{
		BasePath:              "/files/",
		StoreComposer:         composer,
		NotifyCompleteUploads: true,
		MaxSize:               cfg.MaxUploadBytes,
		PreUploadCreateCallback: func(hook tusd.HookEvent) (tusd.HTTPResponse, tusd.FileInfoChanges, error) {
			// enforce auth + size early via hook response (HookEvent carries headers, not *http.Request)
			if subtle.ConstantTimeCompare([]byte(hook.HTTPRequest.Header.Get("X-Api-Key")), []byte(s.Cfg.APIKey)) != 1 {
				return tusd.HTTPResponse{StatusCode: 401, Body: "unauthorized"}, tusd.FileInfoChanges{}, nil
			}
			return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("tusd handler: %w", err)
	}
	go s.watchUploads(handler)

	tusHandler := http.StripPrefix("/files/", handler)
	s.Mux.Handle("/files/", s.wrap(func(w http.ResponseWriter, r *http.Request) {
		tusHandler.ServeHTTP(w, r)
	}, false))
	s.Mux.Handle("/files", s.wrap(func(w http.ResponseWriter, r *http.Request) {
		http.StripPrefix("/files", handler).ServeHTTP(w, r)
	}, false))

	s.Mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	s.Mux.Handle("/assets/{id}", s.wrap(s.handleAsset, true))
	s.Mux.Handle("/assets", s.wrap(s.handleListAssets, true))
	s.Mux.Handle("/exports", s.wrap(s.handleCreateExport, true))
	s.Mux.Handle("/exports/{id}", s.wrap(s.handleGetExport, true))
	s.Mux.Handle("/exports/{id}/events", s.wrap(s.handleEvents, true))
	s.Mux.Handle("/exports/{id}/download", s.wrap(s.handleDownload, true))
	s.Mux.Handle("/metrics", s.wrap(s.handleMetrics, true))
	return s, nil
}

func (s *Server) checkKey(r *http.Request) bool {
	want := s.Cfg.APIKey
	got := r.Header.Get("X-API-Key")
	if got == "" {
		// allow ?api_key= for SSE (EventSource can't set headers)
		got = r.URL.Query().Get("api_key")
	}
	if got == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// wrap adds request_id logging, timeouts, auth, CORS, security headers, rate limit, body cap.
func (s *Server) wrap(next http.HandlerFunc, needAuth bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := uuid.NewString()
		ctx := context.WithValue(r.Context(), "request_id", reqID)
		r = r.WithContext(ctx)
		log := slog.With("request_id", reqID, "method", r.Method, "path", r.URL.Path)
		if needAuth && !s.checkKey(r) {
			writeErr(w, reqID, 401, "unauthorized", "bad or missing API key")
			return
		}
		if !s.rl.allow() {
			writeErr(w, reqID, 429, "rate_limited", "slow down")
			return
		}
		// strict CORS allowlist
		origin := r.Header.Get("Origin")
		if origin != "" {
			for _, o := range s.Cfg.CORSOrigins {
				if strings.TrimSpace(o) == origin {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					break
				}
			}
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB JSON cap
		log.Info("request")
		next.ServeHTTP(w, r)
	})
}

// ---- tusd complete hook: validate by content, create asset, enqueue proxy ----

func (s *Server) watchUploads(handler *tusd.Handler) {
	for ev := range handler.CompleteUploads {
		reqID := uuid.NewString()
		log := slog.With("request_id", reqID, "upload_id", ev.Upload.ID)
		// server-generated storage path only; never trust client filename
		src := filepath.Join(s.Cfg.TusdDir, ev.Upload.ID)
		clean := filepath.Clean(src)
		base := filepath.Clean(s.Cfg.TusdDir)
		if clean != src || !(strings.HasPrefix(clean, base+string(os.PathSeparator)) || clean == base) {
			log.Error("path traversal blocked")
			continue
		}
		info, err := os.Stat(src)
		if err != nil {
			// filestore may use .bin suffix in some versions; try glob
			matches, _ := filepath.Glob(filepath.Join(s.Cfg.TusdDir, ev.Upload.ID+"*"))
			log.Error("upload file not found", "err", err, "matches", fmt.Sprint(matches))
			continue
		}
		clientName, _ := ev.Upload.MetaData["filename"]
		_ = clientName // metadata only, never a path
		if info.Size() > s.Cfg.MaxUploadBytes {
			log.Error("upload too large")
			continue
		}
		if err := media.MagicBytes(src); err != nil {
			log.Error("magic bytes rejected", "err", err)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		p, err := media.ProbeFile(ctx, s.Cfg.FFprobeBin, src)
		cancel()
		if err != nil {
			log.Error("ffprobe failed", "err", err)
			continue
		}
		if err := media.ValidateProbe(p, s.Cfg.MaxDurationMs, s.Cfg.MaxWidth, s.Cfg.MaxHeight, s.Cfg.MaxStreams); err != nil {
			// store rejected asset row for visibility
			id, _ := s.Store.CreateAsset(context.Background(), "", info.Size())
			if id != "" {
				_ = s.Store.MarkAssetRejected(context.Background(), id, err.Error())
			}
			log.Error("validation rejected", "err", err)
			continue
		}
		// move to content-addressed storage path (UUID, fixed base dir)
		// single UUID used for both the row id and the file name.
		assetID := uuid.NewString()
		dst := filepath.Join(s.Cfg.StorageDir, "originals", assetID+".mp4")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			log.Error("mkdir", "err", err)
			continue
		}
		// copy (tusd store owns src; keep src until asset row committed)
		if err := copyFile(src, dst); err != nil {
			log.Error("copy", "err", err)
			continue
		}
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		rowID, err := s.Store.CreateAssetWithID(ctx2, assetID, dst, info.Size())
		cancel2()
		if err != nil {
			log.Error("create asset", "err", err)
			continue
		}
		ctx3, cancel3 := context.WithTimeout(context.Background(), 10*time.Second)
		err = s.Store.MarkAssetValidated(ctx3, rowID, p.DurationMs, p.Width, p.Height, p.VideoCodec)
		cancel3()
		if err != nil {
			log.Error("mark validated", "err", err)
			continue
		}
		if _, err := s.Client.Insert(context.Background(), jobs.ProxyArgs{AssetID: rowID}, &river.InsertOpts{Queue: river.QueueDefault, MaxAttempts: 5}); err != nil {
			log.Error("enqueue proxy", "err", err)
			continue
		}
		log.Info("asset validated, proxy queued", "asset_id", rowID)
	}
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

// ---- handlers ----

type createExportReq struct {
	AssetID      string `json:"asset_id"`
	ChunkSeconds *int   `json:"chunk_seconds,omitempty"`
}

func (s *Server) handleCreateExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeErr(w, reqID(r), 405, "method_not_allowed", "POST only")
		return
	}
	var req createExportReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, reqID(r), 400, "bad_request", "invalid JSON")
		return
	}
	if req.AssetID == "" {
		writeErr(w, reqID(r), 400, "bad_request", "asset_id required")
		return
	}
	a, err := s.Store.GetAsset(r.Context(), req.AssetID)
	if err != nil {
		writeErr(w, reqID(r), 404, "not_found", "asset not found")
		return
	}
	if a.Status != "validated" && a.Status != "proxy_ready" {
		writeErr(w, reqID(r), 409, "conflict", "asset not validated (status="+a.Status+")")
		return
	}
	id, err := s.Store.CreateExport(r.Context(), req.AssetID)
	if err != nil {
		writeErr(w, reqID(r), 500, "internal", err.Error())
		return
	}
	if _, err := s.Client.Insert(r.Context(), jobs.PlanArgs{ExportID: id, AssetID: req.AssetID}, &river.InsertOpts{Queue: river.QueueDefault, MaxAttempts: 5}); err != nil {
		writeErr(w, reqID(r), 500, "internal", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "planned"})
}

func (s *Server) handleGetExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeErr(w, reqID(r), 405, "method_not_allowed", "GET only")
		return
	}
	id := r.PathValue("id")
	exp, err := s.Store.GetExport(r.Context(), id)
	if err != nil {
		writeErr(w, reqID(r), 404, "not_found", "export not found")
		return
	}
	chunks, err := s.Store.ListChunks(r.Context(), id)
	if err != nil {
		writeErr(w, reqID(r), 500, "internal", err.Error())
		return
	}
	done := 0
	for _, c := range chunks {
		if c.Status == "done" {
			done++
		}
	}
	pct := 0.0
	if len(chunks) > 0 {
		pct = float64(done) / float64(len(chunks)) * 100
	} else if exp.Status == "done" {
		pct = 100
	}
	type chunkView struct {
		Idx int `json:"idx"`
		StartMs int64 `json:"start_ms"`
		EndMs int64 `json:"end_ms"`
		Status string `json:"status"`
		Attempts int `json:"attempts"`
	}
	cv := []chunkView{}
	for _, c := range chunks {
		cv = append(cv, chunkView{c.Idx, c.StartMs, c.EndMs, c.Status, c.Attempts})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": exp.ID, "asset_id": exp.AssetID, "status": exp.Status,
		"chunk_count": exp.ChunkCount, "done": done, "percent": pct, "chunks": cv,
	})
}

// SSE with Last-Event-ID resume: replay current chunk states then poll.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetExport(r.Context(), id); err != nil {
		writeErr(w, reqID(r), 404, "not_found", "export not found")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, reqID(r), 500, "internal", "streaming unsupported")
		return
	}
	lastID := 0
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		fmt.Sscanf(v, "%d", &lastID)
	}
	seq := lastID
	send := func(event string, data any) {
		seq++
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", seq, event, string(b))
		fl.Flush()
	}
	// initial dump = resume source of truth (DB)
	chunks, _ := s.Store.ListChunks(r.Context(), id)
	for _, c := range chunks {
		seq++
		b, _ := json.Marshal(map[string]any{"idx": c.Idx, "status": c.Status, "attempts": c.Attempts})
		fmt.Fprintf(w, "id: %d\nevent: chunk\ndata: %s\n\n", seq, string(b))
	}
	fl.Flush()
	tick := time.NewTicker(700 * time.Millisecond)
	defer tick.Stop()
	seen := map[int]string{}
	for _, c := range chunks {
		seen[c.Idx] = c.Status
	}
	timeout := time.After(10 * time.Minute)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-timeout:
			return
		case <-tick.C:
			exp, err := s.Store.GetExport(r.Context(), id)
			if err != nil {
				return
			}
			ch, err := s.Store.ListChunks(r.Context(), id)
			if err != nil {
				return
			}
			for _, c := range ch {
				if seen[c.Idx] != c.Status {
					seen[c.Idx] = c.Status
					send("chunk", map[string]any{"idx": c.Idx, "status": c.Status, "attempts": c.Attempts})
				}
			}
			done := 0
			for _, c := range ch {
				if c.Status == "done" {
					done++
				}
			}
			pct := 0.0
			if len(ch) > 0 {
				pct = float64(done) / float64(len(ch)) * 100
			}
			send("progress", map[string]any{"status": exp.Status, "done": done, "total": len(ch), "percent": pct})
			if exp.Status == "done" || exp.Status == "failed" || exp.Status == "canceled" {
				send("terminal", map[string]any{"status": exp.Status})
				return
			}
		}
	}
}

func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeErr(w, reqID(r), 405, "method_not_allowed", "GET only")
		return
	}
	a, err := s.Store.GetAsset(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, reqID(r), 404, "not_found", "asset not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": a.ID, "status": a.Status, "duration_ms": a.DurationMs,
		"width": a.Width, "height": a.Height, "codec": a.Codec, "size_bytes": a.SizeBytes,
		"proxy_ready": a.Status == "proxy_ready",
	})
}

func (s *Server) handleListAssets(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeErr(w, reqID(r), 405, "method_not_allowed", "GET only")
		return
	}
	rows, err := s.Store.Pool.Query(r.Context(), `SELECT id,status,duration_ms,width,height,codec,size_bytes,created_at FROM assets ORDER BY created_at DESC LIMIT 50`)
	if err != nil {
		writeErr(w, reqID(r), 500, "internal", err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, status string
		var dur, sz *int64
		var wh, ht *int
		var codec *string
		var created time.Time
		if err := rows.Scan(&id, &status, &dur, &wh, &ht, &codec, &sz, &created); err != nil {
			continue
		}
		out = append(out, map[string]any{"id": id, "status": status, "duration_ms": dur, "width": wh, "height": ht, "codec": codec, "size_bytes": sz, "created_at": created})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"assets": out})
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {	exp, err := s.Store.GetExport(r.Context(), r.PathValue("id"))
	if err != nil || exp.OutputPath == nil {
		writeErr(w, reqID(r), 404, "not_found", "output not ready")
		return
	}
	// prefix check (never serve storage dir statically)
	clean := filepath.Clean(*exp.OutputPath)
	base := filepath.Clean(s.Cfg.StorageDir)
	if !(strings.HasPrefix(clean, base+string(os.PathSeparator)) || clean == base) {
		writeErr(w, reqID(r), 403, "forbidden", "bad path")
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="export.mp4"`)
	http.ServeFile(w, r, clean)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func reqID(r *http.Request) string {
	if v, ok := r.Context().Value("request_id").(string); ok {
		return v
	}
	return "-"
}

// simple token-bucket-ish limiter
type rateLimiter struct {
	mu     sync.Mutex
	rps    int
	window time.Time
	count  int
}

func newRateLimiter(rps int) *rateLimiter { return &rateLimiter{rps: rps} }

func (l *rateLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.window) > time.Second {
		l.window = now
		l.count = 0
	}
	l.count++
	return l.count <= l.rps*2
}
