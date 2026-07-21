package live

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
)

// Loudness is a track's stored loudnorm measurement (ingest measures, never
// rewrites — see internal/ingest/probe.go). It drives one-pass LINEAR
// normalization at decode time.
type Loudness struct{ I, TP, LRA float64 }

// DecodeArgs builds the per-track batch decode: seek (resume), loudnorm with
// the stored measurements, raw s16le 48k stereo on stdout. No -re — pacing
// is the feeder's job, which keeps its sample clock authoritative.
func DecodeArgs(path string, l Loudness, offsetS float64) []string {
	args := []string{"-hide_banner", "-nostats"}
	if offsetS > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", offsetS))
	}
	return append(args,
		"-i", path,
		"-af", fmt.Sprintf(
			"loudnorm=I=-14:TP=-1.5:LRA=11:measured_I=%.1f:measured_TP=%.1f:measured_LRA=%.1f:linear=true",
			l.I, l.TP, l.LRA),
		"-f", "s16le", "-ar", "48000", "-ac", "2", "pipe:1")
}

// EncodeArgs builds the persistent session encoder: paced PCM on stdin, AAC
// 128k, HLS with a sliding window and PROGRAM-DATE-TIME.
func EncodeArgs(dir string, segmentSeconds int) []string {
	return []string{"-hide_banner", "-nostats",
		"-f", "s16le", "-ar", "48000", "-ac", "2", "-i", "pipe:0",
		"-c:a", "aac", "-b:a", "128k",
		"-f", "hls",
		"-hls_time", fmt.Sprintf("%d", segmentSeconds),
		"-hls_list_size", "6",
		"-hls_flags", "delete_segments+program_date_time",
		"-hls_segment_filename", filepath.Join(dir, "seg-%d.ts"),
		filepath.Join(dir, "live.m3u8")}
}

type Decoder interface {
	Open(ctx context.Context, path string, l Loudness, offsetS float64) (io.ReadCloser, error)
}

type Session interface {
	Stdin() io.WriteCloser
	Done() <-chan error
	Stop()
}

type Encoder interface {
	Start(ctx context.Context, dir string) (Session, error)
}

// FFDecoder shells out to ffmpeg for one track's PCM.
type FFDecoder struct{}

func NewFFDecoder() *FFDecoder { return &FFDecoder{} }

// decodeReader wraps stdout; Close kills the process and reaps it.
type decodeReader struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (d *decodeReader) Close() error {
	_ = d.ReadCloser.Close()
	_ = d.cmd.Process.Kill()
	_ = d.cmd.Wait()
	return nil
}

func (FFDecoder) Open(ctx context.Context, path string, l Loudness, offsetS float64) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", DecodeArgs(path, l, offsetS)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg decode start: %w", err)
	}
	return &decodeReader{ReadCloser: out, cmd: cmd}, nil
}

// FFEncoder runs the persistent session encoder.
type FFEncoder struct{}

func NewFFEncoder() *FFEncoder { return &FFEncoder{} }

type ffSession struct {
	stdin io.WriteCloser
	done  chan error
	cmd   *exec.Cmd
}

func (s *ffSession) Stdin() io.WriteCloser { return s.stdin }
func (s *ffSession) Done() <-chan error    { return s.done }
func (s *ffSession) Stop() {
	_ = s.stdin.Close()
	<-s.done
}

func (FFEncoder) Start(ctx context.Context, dir string) (Session, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", EncodeArgs(dir, 6)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg encode start: %w", err)
	}
	s := &ffSession{stdin: stdin, done: make(chan error, 1), cmd: cmd}
	go func() {
		err := cmd.Wait()
		if err != nil && stderr.Len() > 0 {
			err = fmt.Errorf("%w — %.300s", err, stderr.String())
		}
		s.done <- err
		close(s.done)
	}()
	return s, nil
}
