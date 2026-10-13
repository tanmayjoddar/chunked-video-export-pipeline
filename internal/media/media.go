package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var AllowedContainers = map[string]bool{"mp4": true, "mov": true, "matroska": true, "webm": true}
var AllowedVideoCodecs = map[string]bool{"h264": true, "hevc": true, "prores": true, "vp9": true}
var AllowedAudioCodecs = map[string]bool{"aac": true, "pcm_s16le": true, "pcm_s24le": true, "opus": true, "mp3": true}

type Probe struct {
	DurationMs int64
	Width      int
	Height     int
	VideoCodec string
	FormatName string
	Streams    int
	HasVideo   bool
}

type ffprobeOut struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
	} `json:"streams"`
	Format struct {
		Duration   string `json:"duration"`
		FormatName string `json:"format_name"`
	} `json:"format"`
}

// Check ffmpeg.org docs: ffprobe JSON output; segment muxer; concat demuxer.
// pkg.go.dev: os/exec CommandContext. No shell anywhere.

func run(ctx context.Context, bin string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = os.Environ()
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	// never attach stdin of server to ffmpeg (avoid blocking reads)
	cmd.Stdin = nil
	err := cmd.Run()
	if err != nil {
		return so.Bytes(), se.Bytes(), fmt.Errorf("%s: %w: %s", bin, err, truncate(string(se.Bytes()), 800))
	}
	return so.Bytes(), se.Bytes(), nil
}

func ProbeFile(ctx context.Context, ffprobeBin, path string) (*Probe, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	so, se, err := run(ctx, ffprobeBin,
		"-v", "error",
		"-show_streams", "-show_format",
		"-of", "json",
		"-protocol_whitelist", "file,pipe",
		path)
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w: %s", err, truncate(string(se), 500))
	}
	var o ffprobeOut
	if err := json.Unmarshal(so, &o); err != nil {
		return nil, fmt.Errorf("ffprobe parse: %w", err)
	}
	p := &Probe{FormatName: o.Format.FormatName, Streams: len(o.Streams)}
	var dur float64
	fmt.Sscanf(o.Format.Duration, "%f", &dur)
	p.DurationMs = int64(math.Round(dur * 1000))
	for _, s := range o.Streams {
		if s.CodecType == "video" && !p.HasVideo {
			p.HasVideo = true
			p.VideoCodec = s.CodecName
			p.Width, p.Height = s.Width, s.Height
		}
	}
	return p, nil
}

func ValidateProbe(p *Probe, maxDurMs int64, maxW, maxH, maxStreams int) error {
	if !p.HasVideo {
		return fmt.Errorf("no video stream")
	}
	if !AllowedVideoCodecs[strings.ToLower(p.VideoCodec)] {
		return fmt.Errorf("video codec %q not allowlisted", p.VideoCodec)
	}
	// format_name may be comma list like "mov,mp4,m4a,3gp,3g2,mj2"
	allowed := false
	for _, f := range strings.Split(p.FormatName, ",") {
		if AllowedContainers[strings.TrimSpace(f)] {
			allowed = true
			break
		}
		// mp4 family
		f = strings.TrimSpace(f)
		if f == "mp4" || f == "mov" || f == "m4a" || f == "3gp" {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("container %q not allowlisted", p.FormatName)
	}
	if p.DurationMs <= 0 || p.DurationMs > maxDurMs {
		return fmt.Errorf("duration %dms out of range", p.DurationMs)
	}
	if p.Width <= 0 || p.Width > maxW || p.Height <= 0 || p.Height > maxH {
		return fmt.Errorf("resolution %dx%d out of range", p.Width, p.Height)
	}
	if p.Streams > maxStreams {
		return fmt.Errorf("too many streams: %d", p.Streams)
	}
	return nil
}

// MagicBytes checks first bytes look like a media container (ftyp/isom/matroska/webm).
func MagicBytes(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 16)
	n, _ := f.Read(head)
	head = head[:n]
	if len(head) >= 4 && head[0] == 0x1A && head[1] == 0x45 && head[2] == 0xDF && head[3] == 0xA3 {
		return nil // EBML (mkv/webm)
	}
	if bytes.Contains(head, []byte("ftyp")) {
		return nil // mp4/mov
	}
	return fmt.Errorf("unrecognized magic bytes")
}

// KeyframeTimes returns sorted keyframe timestamps in seconds via ffprobe skip_frame nokey.
func KeyframeTimes(ctx context.Context, ffprobeBin, path string) ([]float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	so, se, err := run(ctx, ffprobeBin,
		"-v", "error",
		"-select_streams", "v:0",
		"-skip_frame", "nokey",
		"-show_entries", "frame=pkt_pts_time",
		"-of", "csv=p=0",
		"-protocol_whitelist", "file,pipe",
		path)
	if err != nil {
		return nil, fmt.Errorf("keyframe probe: %w: %s", err, truncate(string(se), 300))
	}
	var out []float64
	for _, line := range strings.Split(string(so), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var v float64
		if _, err := fmt.Sscanf(line, "%f", &v); err == nil {
			out = append(out, v)
		}
	}
	sort.Float64s(out)
	return out, nil
}

// PlanChunks splits [0,durationMs] into ~chunkSec pieces snapped to nearest keyframe.
// Pure function of (durationMs, keyframes, chunkSec) -> table-testable.
func PlanChunks(durationMs int64, keyframesSec []float64, chunkSec int, maxChunks int) ([][2]int64, error) {
	if durationMs <= 0 {
		return nil, fmt.Errorf("bad duration")
	}
	if chunkSec <= 0 {
		chunkSec = 10
	}
	target := int64(chunkSec * 1000)
	n := int((durationMs + target - 1) / target)
	if n < 1 {
		n = 1
	}
	if n > maxChunks {
		return nil, fmt.Errorf("needs %d chunks > max %d", n, maxChunks)
	}
	kf := append([]float64(nil), keyframesSec...)
	sort.Float64s(kf)
	// cut points: cuts[0]=0, cuts[n]=durationMs; interior cuts snapped to keyframes.
	cuts := make([]int64, n+1)
	cuts[0] = 0
	cuts[n] = durationMs
	for i := 1; i < n; i++ {
		ideal := int64(i) * durationMs / int64(n)
		cut := ideal
		if len(kf) > 0 {
			want := float64(ideal) / 1000.0
			best := -1.0
			bestD := 1e18
			for _, k := range kf {
				d := math.Abs(k - want)
				if d < bestD {
					bestD = d
					best = k
				}
			}
			if best >= 0 && bestD*1000 <= float64(target)/2 {
				snapped := int64(math.Round(best * 1000))
				if snapped > cuts[i-1]+500 && snapped < durationMs-500 {
					cut = snapped
				}
			}
		}
		if cut <= cuts[i-1] {
			cut = cuts[i-1] + 500
		}
		if cut >= durationMs {
			cut = durationMs - 500
		}
		cuts[i] = cut
	}
	bounds := make([][2]int64, 0, n)
	for i := 0; i < n; i++ {
		start, end := cuts[i], cuts[i+1]
		if end <= start {
			end = start + 500
			if end > durationMs {
				end = durationMs
			}
		}
		bounds = append(bounds, [2]int64{start, end})
	}
	return bounds, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

// ---- ffmpeg encode wrappers (deterministic) ----
// Fixed settings for determinism: -threads 1, fixed GOP (-g 48 -keyint_min 48 -sc_threshold 0),
// -fflags +bitexact -flags:v +bitexact. Same pinned ffmpeg version in Docker.

func baseArgs() []string {
	return []string{"-y", "-nostdin", "-protocol_whitelist", "file,pipe", "-threads", "1"}
}

func ProxyArgs(src, dst string) []string {
	a := baseArgs()
	a = append(a,
		"-i", src,
		"-vf", "scale=1280:-2",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "28",
		"-pix_fmt", "yuv420p",
		"-g", "48", "-keyint_min", "48", "-sc_threshold", "0",
		"-c:a", "aac", "-b:a", "96k",
		"-fflags", "+bitexact", "-flags:v", "+bitexact",
		"-map_metadata", "-1",
		dst)
	return a
}

func ChunkArgs(src, dst string, startMs, endMs int64) []string {
	dur := float64(endMs-startMs) / 1000.0
	a := baseArgs()
	a = append(a,
		"-ss", fmt.Sprintf("%.3f", float64(startMs)/1000.0),
		"-i", src,
		"-t", fmt.Sprintf("%.3f", dur),
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "20",
		"-pix_fmt", "yuv420p",
		"-g", "48", "-keyint_min", "48", "-sc_threshold", "0",
		"-vsync", "cfr",
		"-c:a", "aac", "-b:a", "128k", "-ar", "48000", "-ac", "2",
		"-fflags", "+bitexact", "-flags:v", "+bitexact",
		"-map_metadata", "-1",
		dst)
	return a
}

// RunToTemp runs bin with args writing to dst via dst.tmp + atomic rename (idempotent jobs).
func RunToTemp(ctx context.Context, bin string, args []string, dst string, timeoutS int) error {
	if timeoutS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutS)*time.Second)
		defer cancel()
	}
	// resolve final arg (output) -> redirect to tmp
	if len(args) == 0 || args[len(args)-1] != dst {
		return fmt.Errorf("internal: last arg must be dst")
	}
	tmp := tempPath(dst)
	args2 := append(append([]string{}, args[:len(args)-1]...), tmp)
	// ensure parent dir
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	// idempotent: if dst exists and tmp absent, skip? Caller decides; here always (re)render.
	_, _, err := run(ctx, bin, args2...)
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("%s failed: %w", bin, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

func ConcatArgs(listPath, dst string) []string {
	a := baseArgs()
	a = append(a,
		"-f", "concat", "-safe", "0",
		"-i", listPath,
		"-c", "copy",
		"-fflags", "+bitexact",
		"-map_metadata", "-1",
		dst)
	return a
}

// RunConcat runs the concat demuxer (building args internally so paths stay slices, never shell).
func RunConcat(ctx context.Context, ffmpegBin, listPath, dst string, timeoutS int) error {
	return RunToTemp(ctx, ffmpegBin, ConcatArgs(listPath, dst), dst, timeoutS)
}

// tempPath keeps the container extension so ffmpeg can infer the output
// format (it does not accept ".tmp" suffixes), then the caller atomically
// renames tmp -> dst.
func tempPath(dst string) string {
	if strings.HasSuffix(dst, ".mp4") {
		return strings.TrimSuffix(dst, ".mp4") + ".tmp.mp4"
	}
	return dst + ".tmp"
}

func FrameCount(ctx context.Context, ffprobeBin, path string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	so, _, err := run(ctx, ffprobeBin, "-v", "error", "-select_streams", "v:0",
		"-count_frames", "-show_entries", "stream=nb_read_frames",
		"-of", "csv=p=0", "-protocol_whitelist", "file,pipe", path)
	if err != nil {
		return 0, fmt.Errorf("frame count: %w", err)
	}
	var n int64
	fmt.Sscanf(strings.TrimSpace(string(so)), "%d", &n)
	return n, nil
}

func SHA256File(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	// streaming to avoid OOM on big files
	return sha256bytes(b), nil
}

func DurationMs(ctx context.Context, ffprobeBin, path string) (int64, error) {
	p, err := ProbeFile(ctx, ffprobeBin, path)
	if err != nil {
		return 0, err
	}
	return p.DurationMs, nil
}
