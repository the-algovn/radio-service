package pcm

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sort"
)

const (
	// windowMS is the envelope resolution. 50ms is short enough to place a
	// cue within a fifth of the shortest fade anyone would use, and long
	// enough that a single quiet sample between two loud ones cannot end a
	// run.
	windowMS = 50
	// silenceFloorDBFS is what counts as "nothing left". Deliberately well
	// below a fade's audible tail: anything above this is decay, and only
	// decay is allowed to be ambiguous. -50 matches the floor
	// internal/director/render.go already uses to detect an all-silent take.
	silenceFloorDBFS = -50.0
	// decayTolerance lets a window sit slightly above its successor without
	// ending the monotonic run — real fades are not perfectly monotonic and
	// a strict comparison would truncate almost every one of them at the
	// first bump.
	decayTolerance = 1.5 // dB
	// minDecayDropDB is the smallest total decline, from the loud end of a
	// candidate run to its quiet end, that counts as a real fade rather than
	// noise. It exists because "non-increasing" is a much weaker claim than
	// "declining": a constant-level tone is non-increasing at every window
	// (each one is <= its predecessor plus tolerance, with room to spare),
	// so the walk below cannot by itself tell a cold, flat ending from a
	// slow fade — both satisfy the comparison. Requiring the run's total
	// drop to clear this floor is what actually distinguishes them.
	minDecayDropDB = 6.0 // dB
	// decaySmoothRadius is how many windows on each side go into the median
	// used to find the decay run (see smoothLevels). Small on purpose: real
	// per-window jitter lives on a 100-300ms scale, so a 5-tap (±100ms)
	// median damps it without blurring a decline that plays out over many
	// seconds into something else. Measured against real production audio:
	// larger radii (1s+) started pulling in unrelated loud material from
	// elsewhere in the track and reported tens to hundreds of seconds of
	// "decay" — the fix for one failure mode is not "smooth more".
	decaySmoothRadius = 2 // windows
)

// TailCues measures how much of a stream's ending is safe to overlap.
//
// It returns two DISJOINT runs, both measured backwards: tailSilenceS is the
// trailing run below the silence floor, and tailDecayS is the monotonic
// decline immediately before it. The decay scan deliberately begins at the
// start of the silence rather than at EOF — measured from EOF it would
// re-count the silence, and the sum (which is what a crossfade budget uses)
// would double-count. That is not a theoretical concern: the buggy form
// returns zero budget for a long fade-out, the exact ending a crossfade most
// wants to use.
//
// The input is interleaved s16le. r is consumed to EOF and never seeked, so
// this works on an ffmpeg pipe.
func TailCues(r io.Reader, sampleRate, channels int) (tailSilenceS, tailDecayS float64, err error) {
	if sampleRate <= 0 || channels <= 0 {
		return 0, 0, errors.New("pcm: sampleRate and channels must be positive")
	}
	frameBytes := channels * SampleBytes
	windowFrames := sampleRate * windowMS / 1000
	windowBytes := windowFrames * frameBytes
	if windowBytes == 0 {
		// sampleRate*windowMS/1000 truncates to 0 below ~20Hz. Left
		// unchecked, io.ReadFull on a zero-length buffer returns (0, nil)
		// forever — neither loop exit below ever fires and TailCues hangs.
		// Unreachable with any real sample rate, but the geometry check
		// exists precisely to reject bad geometry, so this belongs here.
		return 0, 0, errors.New("pcm: sampleRate too low for windowMS resolution")
	}

	// One pass, accumulating one dBFS value per window. A track is minutes
	// long, so this is a few thousand float64s — cheaper than buffering the
	// audio and far cheaper than a second decode.
	var levels []float64
	buf := make([]byte, windowBytes)
	for {
		n, rerr := io.ReadFull(r, buf)
		if n >= frameBytes {
			levels = append(levels, windowDBFS(buf[:n-n%frameBytes]))
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
				break
			}
			return 0, 0, rerr
		}
	}
	if len(levels) == 0 {
		return 0, 0, nil
	}

	windowS := float64(windowMS) / 1000

	// Run 1: trailing silence, from the end.
	sil := len(levels)
	for sil > 0 && levels[sil-1] <= silenceFloorDBFS {
		sil--
	}
	tailSilenceS = float64(len(levels)-sil) * windowS

	// Run 2: monotonic decline ending where the silence began — NOT at EOF.
	//
	// The walk compares against a median-smoothed series, not the raw
	// per-window levels used for Run 1. Real audio's RMS jitters several dB
	// between adjacent 50ms windows even while genuinely declining overall —
	// measured on production tracks, a window can sit 2-4dB above its own
	// predecessor in the middle of a 20+ second fade into silence. A
	// same-magnitude decayTolerance cannot absorb that: comparing raw
	// windows one-to-the-next made the walk break after a single step,
	// every time, on real audio, even though the whole point of the
	// tolerance is to survive exactly this kind of non-monotonic bump. The
	// median damps that jitter while leaving a genuine multi-second decline
	// intact, because the trend, unlike the jitter, persists across
	// neighboring windows.
	smoothed := smoothLevels(levels)
	dec := sil
	for dec > 1 && smoothed[dec-1] <= smoothed[dec-2]+decayTolerance {
		dec--
	}
	// The walk above will happily claim an entire flat, cold ending: every
	// window there is non-increasing relative to the last (it's equal to
	// it), so nothing ever breaks the loop. That is not decay — nothing is
	// declining — so gate on the run's actual drop rather than its length.
	if sil-dec >= 1 && smoothed[dec]-smoothed[sil-1] >= minDecayDropDB {
		// The tolerant walk's own left boundary is not trustworthy as the
		// run's start: the same slack that survives one dither bump chains
		// through an unbroken run of them, so a merely near-flat lead-in —
		// any real held pad or room tone, not just a bit-exact synthetic
		// tone — rides along too, inflating the reported run far past the
		// actual decline. Re-anchor to the run's own peak instead: walk
		// forward from the gate-approved start only while the level is
		// still within decayTolerance of the loudest window actually in
		// the run. That can only shorten the run — it never disqualifies
		// one the gate already approved — and it biases the reported
		// start slightly late, 1.5dB into the decline, which is the safe
		// direction for a crossfade budget to be wrong in.
		peak := smoothed[dec]
		for _, lv := range smoothed[dec:sil] {
			if lv > peak {
				peak = lv
			}
		}
		for dec < sil-1 && smoothed[dec] >= peak-decayTolerance {
			dec++
		}
		tailDecayS = float64(sil-dec) * windowS
	}

	return tailSilenceS, tailDecayS, nil
}

// smoothLevels returns the windowed median of levels, radius windows on
// each side (clipped at the edges). A median, not a mean: a single window
// of true digital silence (-Inf) inside an otherwise-declining passage would
// drag a mean straight to -Inf and stay there for the rest of the smoothing
// window, but the median just ignores that one outlier as long as it isn't
// the majority of the window.
func smoothLevels(levels []float64) []float64 {
	out := make([]float64, len(levels))
	window := make([]float64, 0, 2*decaySmoothRadius+1)
	for i := range levels {
		lo, hi := i-decaySmoothRadius, i+decaySmoothRadius
		if lo < 0 {
			lo = 0
		}
		if hi >= len(levels) {
			hi = len(levels) - 1
		}
		window = append(window[:0], levels[lo:hi+1]...)
		sort.Float64s(window)
		out[i] = window[len(window)/2]
	}
	return out
}

// windowDBFS is the RMS of one window expressed in dBFS. Digital silence
// returns math.Inf(-1), which compares below any floor without a special case.
func windowDBFS(b []byte) float64 {
	n := len(b) / SampleBytes
	if n == 0 {
		return math.Inf(-1)
	}
	var sum float64
	for i := range n {
		v := float64(int16(binary.LittleEndian.Uint16(b[i*SampleBytes:])))
		sum += v * v
	}
	rms := math.Sqrt(sum/float64(n)) / 32768
	if rms <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(rms)
}
