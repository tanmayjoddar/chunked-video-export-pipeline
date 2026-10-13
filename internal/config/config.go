package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	DatabaseURL       string
	APIKey            string
	Port              string
	StorageDir        string
	TusdDir           string
	MaxUploadBytes    int64
	MaxDurationMs     int64
	MaxWidth          int
	MaxHeight         int
	MaxStreams        int
	MaxChunks         int
	ChunkSeconds      int
	CORSOrigins       []string
	RateLimitRPS      int
	FFmpegBin         string
	FFprobeBin        string
	RiverMaxWorkers   int
	RescueStuckAfterS int
	JobTimeoutS       int
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getint(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func getint64(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func Load() (*Config, error) {
	c := &Config{
		DatabaseURL:       getenv("DATABASE_URL", "postgres://app:app@postgres:5432/exports?sslmode=disable"),
		APIKey:            os.Getenv("API_KEY"),
		Port:              getenv("PORT", "8080"),
		StorageDir:        getenv("STORAGE_DIR", "/data/media"),
		TusdDir:           getenv("TUSD_DIR", "/data/tusd"),
		MaxUploadBytes:    getint64("MAX_UPLOAD_BYTES", 2*1024*1024*1024),
		MaxDurationMs:     getint64("MAX_DURATION_MS", 600000),
		MaxWidth:          getint("MAX_WIDTH", 3840),
		MaxHeight:         getint("MAX_HEIGHT", 2160),
		MaxStreams:        getint("MAX_STREAMS", 4),
		MaxChunks:         getint("MAX_CHUNKS", 64),
		ChunkSeconds:      getint("CHUNK_SECONDS", 10),
		RateLimitRPS:      getint("RATE_LIMIT_RPS", 20),
		FFmpegBin:         getenv("FFMPEG_BIN", "ffmpeg"),
		FFprobeBin:        getenv("FFPROBE_BIN", "ffprobe"),
		RiverMaxWorkers:   getint("RIVER_MAX_WORKERS", 10),
		RescueStuckAfterS: getint("RESCUE_STUCK_AFTER_S", 60),
		JobTimeoutS:       getint("JOB_TIMEOUT_S", 600),
	}
	origins := getenv("CORS_ORIGINS", "http://localhost:3000")
	c.CORSOrigins = strings.Split(origins, ",")
	if c.APIKey == "" || len(c.APIKey) < 32 || c.APIKey == "change-me" {
		return nil, fmt.Errorf("API_KEY missing or weak: set a >=32 char random key in .env (see .env.example)")
	}
	return c, nil
}
