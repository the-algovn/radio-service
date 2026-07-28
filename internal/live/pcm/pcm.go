// Package pcm is the raw-audio arithmetic behind item transitions: gain
// ramps and saturating mixes over interleaved s16le samples.
//
// Everything here is pure — no I/O, no clock, no allocation — which is why
// it is its own package. internal/live's objective audio assertions are
// //go:build integration and CI runs a bare `go test ./...`, so anything
// that must be verified on every push cannot depend on ffmpeg.
package pcm

import "encoding/binary"

// SampleBytes is one s16le sample. Every function here works sample-wise and
// is therefore agnostic to channel count; the air format is stereo, so one
// frame is two samples.
const SampleBytes = 2

// Linear returns the crossfade gain pair at position p in [0,1], clamped.
//
// The gains sum to 1 at every p, which makes the mix clip-free by
// construction: |gOut*a + gIn*b| <= max(|a|,|b|). This is not a stylistic
// choice. Every track airs pinned at roughly -1.5 dBTP, and an equal-power
// (sqrt) curve at unity gain was measured breaching that ceiling on ~26% of
// real seam alignments and hard-clipping on 1.1-1.7% of them. Linear's cost
// is a ~3 dB power dip mid-fade on uncorrelated material — perceptually
// nearer 1-1.5 dB — which is the correct trade against an audible click.
func Linear(p float64) (gOut, gIn float32) {
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	return float32(1 - p), float32(p)
}

// ScaleRamp multiplies buf in place by a gain interpolated linearly from g0
// at the first sample to g1 at the last. A single-sample buffer uses g0. A
// trailing odd byte is left untouched rather than reinterpreted, so a
// misaligned buffer degrades to a dropped sample instead of channel-swapped
// noise.
func ScaleRamp(buf []byte, g0, g1 float32) {
	n := len(buf) / SampleBytes
	if n == 0 {
		return
	}
	for i := range n {
		g := g0
		if n > 1 {
			g = g0 + (g1-g0)*float32(i)/float32(n-1)
		}
		off := i * SampleBytes
		v := float32(int16(binary.LittleEndian.Uint16(buf[off:])))
		binary.LittleEndian.PutUint16(buf[off:], uint16(satRound(v*g)))
	}
}

// AddSat adds src into dst sample-wise, saturating at the int16 rails.
// Samples beyond the shorter slice are ignored.
//
// Saturation is mandatory. int16(int32(a+b)) wraps, and a wrap flips
// polarity — the exact discontinuity transitions exist to remove.
func AddSat(dst, src []byte) {
	n := min(len(dst), len(src)) / SampleBytes
	for i := range n {
		off := i * SampleBytes
		a := int32(int16(binary.LittleEndian.Uint16(dst[off:])))
		b := int32(int16(binary.LittleEndian.Uint16(src[off:])))
		binary.LittleEndian.PutUint16(dst[off:], uint16(sat(a+b)))
	}
}

func sat(v int32) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}

// satRound rounds half away from zero, so a -0.5 product does not creep
// toward positive across a long fade.
func satRound(v float32) int16 {
	if v >= 0 {
		return sat(int32(v + 0.5))
	}
	return sat(int32(v - 0.5))
}
