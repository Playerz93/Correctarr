// Package integrity checks that a media file is really readable through its
// symlink. Three depths: quick (head + tail + container magic), standard
// (ffprobe) and full (stream every byte).
package integrity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	DepthOff      = "off"
	DepthQuick    = "quick"
	DepthStandard = "standard"
	DepthFull     = "full"
)

// Disk is the result of the filesystem check that runs before any read.
type Disk struct {
	Exists     bool   // Lstat succeeded
	IsSymlink  bool   // the path itself is a symlink
	Resolves   bool   // Stat (following links) succeeded
	Size       int64  // size of the resolved target
	Target     string // readlink result when a symlink
	StatError  string
	ResolvedTo string
}

// CheckDisk inspects the path without reading media bytes.
func CheckDisk(path string) Disk {
	var d Disk
	li, err := os.Lstat(path)
	if err != nil {
		d.StatError = shortErr(err)
		return d
	}
	d.Exists = true
	d.IsSymlink = li.Mode()&os.ModeSymlink != 0
	if d.IsSymlink {
		d.Target, _ = os.Readlink(path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		d.StatError = shortErr(err)
		return d
	}
	if fi.IsDir() {
		d.StatError = "path is a directory"
		return d
	}
	d.Resolves = true
	d.Size = fi.Size()
	if d.IsSymlink {
		if r, err := filepath.EvalSymlinks(path); err == nil {
			d.ResolvedTo = r
		}
	}
	return d
}

// shortErr drops the path from a *PathError so the message is not repeated.
func shortErr(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

type Options struct {
	Depth          string
	FfprobeTimeout time.Duration
	ExpectedSize   int64 // from the arr; 0 = unknown
}

// Result of a read-through check.
type Result struct {
	OK     bool
	Depth  string
	Detail string
}

// Check performs the configured depth of read-through verification.
func Check(ctx context.Context, path string, opt Options) Result {
	switch opt.Depth {
	case DepthOff, "":
		return Result{OK: true, Depth: DepthOff}
	case DepthQuick:
		return quick(path)
	case DepthStandard:
		if r := quick(path); !r.OK {
			return r
		}
		return probe(ctx, path, opt.FfprobeTimeout)
	case DepthFull:
		if r := quick(path); !r.OK {
			return r
		}
		if r := probe(ctx, path, opt.FfprobeTimeout); !r.OK {
			return r
		}
		return full(ctx, path, opt.ExpectedSize)
	}
	return Result{OK: false, Depth: opt.Depth, Detail: "unknown integrity depth"}
}

const window = 256 << 10

// quick reads the first and last 256 KiB and validates the container magic.
func quick(path string) Result {
	res := Result{Depth: DepthQuick}
	f, err := os.Open(path)
	if err != nil {
		res.Detail = "open: " + err.Error()
		return res
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		res.Detail = "stat: " + err.Error()
		return res
	}
	size := fi.Size()
	if size == 0 {
		res.Detail = "file is empty"
		return res
	}
	n := int64(window)
	if size < n {
		n = size
	}
	head := make([]byte, n)
	if _, err := io.ReadFull(f, head); err != nil {
		res.Detail = "read head: " + err.Error()
		return res
	}
	if kind, ok := sniff(head); !ok {
		res.Detail = "unrecognised container (" + kind + ")"
		return res
	}
	if size > n {
		tail := make([]byte, n)
		if _, err := f.ReadAt(tail, size-n); err != nil && !errors.Is(err, io.EOF) {
			res.Detail = "read tail: " + err.Error()
			return res
		}
	}
	res.OK = true
	return res
}

// sniff identifies the container type from the first bytes.
func sniff(b []byte) (string, bool) {
	if len(b) < 12 {
		return "too short", false
	}
	switch {
	case bytes.HasPrefix(b, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return "matroska", true
	case string(b[4:8]) == "ftyp" || string(b[4:8]) == "moov" || string(b[4:8]) == "mdat" || string(b[4:8]) == "wide" || string(b[4:8]) == "free":
		return "mp4", true
	case bytes.HasPrefix(b, []byte("RIFF")) && string(b[8:12]) == "AVI ":
		return "avi", true
	case b[0] == 0x47 && len(b) > 376 && b[188] == 0x47 && b[376] == 0x47:
		return "mpeg-ts", true
	case bytes.HasPrefix(b, []byte{0x00, 0x00, 0x01, 0xBA}):
		return "mpeg-ps", true
	case bytes.HasPrefix(b, []byte{0x30, 0x26, 0xB2, 0x75}):
		return "wmv/asf", true
	case bytes.HasPrefix(b, []byte("FLV")):
		return "flv", true
	case bytes.HasPrefix(b, []byte("OggS")):
		return "ogg", true
	}
	// A debrid error page or HTML placeholder is the classic failure.
	low := bytes.ToLower(b[:min(len(b), 512)])
	if bytes.Contains(low, []byte("<html")) || bytes.Contains(low, []byte("<!doctype")) {
		return "html document", false
	}
	if bytes.Count(b[:min(len(b), 4096)], []byte{0}) == min(len(b), 4096) {
		return "all zero bytes", false
	}
	return fmt.Sprintf("magic %x", b[:8]), false
}

// HaveFfprobe reports whether ffprobe is available.
func HaveFfprobe() bool {
	_, err := exec.LookPath("ffprobe")
	return err == nil
}

// probe runs ffprobe and requires a duration and a video stream.
func probe(ctx context.Context, path string, timeout time.Duration) Result {
	res := Result{Depth: DepthStandard}
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	bin, err := exec.LookPath("ffprobe")
	if err != nil {
		res.Detail = "ffprobe not found in PATH"
		return res
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin,
		"-v", "error",
		"-show_entries", "format=duration,format_name:stream=codec_type,codec_name",
		"-of", "json",
		path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if cctx.Err() == context.DeadlineExceeded {
		res.Detail = fmt.Sprintf("ffprobe timed out after %s", timeout)
		return res
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if len(msg) > 200 {
			msg = msg[:200] + "..."
		}
		res.Detail = "ffprobe: " + msg
		return res
	}
	var out struct {
		Format struct {
			Duration   string `json:"duration"`
			FormatName string `json:"format_name"`
		} `json:"format"`
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		res.Detail = "ffprobe output: " + err.Error()
		return res
	}
	dur, _ := strconv.ParseFloat(out.Format.Duration, 64)
	if dur <= 0 {
		res.Detail = "ffprobe: no duration (truncated or bad index)"
		return res
	}
	hasVideo := false
	for _, s := range out.Streams {
		if s.CodecType == "video" {
			hasVideo = true
		}
	}
	if !hasVideo {
		res.Detail = "ffprobe: no video stream"
		return res
	}
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		res.Detail = "ffprobe warnings: " + msg
		res.OK = true
		return res
	}
	res.OK = true
	res.Detail = fmt.Sprintf("%s, %.0fs", out.Format.FormatName, dur)
	return res
}

// full streams the whole file and compares the byte count to the size.
func full(ctx context.Context, path string, expected int64) Result {
	res := Result{Depth: DepthFull}
	f, err := os.Open(path)
	if err != nil {
		res.Detail = "open: " + err.Error()
		return res
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		res.Detail = "stat: " + err.Error()
		return res
	}
	want := fi.Size()
	if expected > 0 && expected != want {
		res.Detail = fmt.Sprintf("size mismatch: arr says %d, disk says %d", expected, want)
		return res
	}
	start := time.Now()
	n, err := io.Copy(io.Discard, &ctxReader{ctx: ctx, r: f})
	if err != nil {
		res.Detail = fmt.Sprintf("read failed after %d of %d bytes: %v", n, want, err)
		return res
	}
	if n != want {
		res.Detail = fmt.Sprintf("short read: %d of %d bytes", n, want)
		return res
	}
	res.OK = true
	res.Detail = fmt.Sprintf("read %.1f GiB in %s", float64(n)/(1<<30), time.Since(start).Round(time.Second))
	return res
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
