package pcm

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
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
	dec := sil
	for dec > 1 && levels[dec-1] <= levels[dec-2]+decayTolerance {
		dec--
	}
	// The walk above will happily claim an entire flat, cold ending: every
	// window there is non-increasing relative to the last (it's equal to
	// it), so nothing ever breaks the loop. That is not decay — nothing is
	// declining — so gate on the run's actual drop rather than its length.
	if sil-dec >= 1 && levels[dec]-levels[sil-1] >= minDecayDropDB {
		// The same slack lets a flat lead-in ride along in front of a real
		// decline (e.g. a held intro before the fade proper starts): those
		// windows are bit-identical to their successor, so they add length
		// to the run without adding any of the drop that justified keeping
		// it. Trim them off the front so the reported run starts where the
		// level actually begins to fall.
		for dec < sil-1 && levels[dec] == levels[dec+1] {
			dec++
		}
		tailDecayS = float64(sil-dec) * windowS
	}

	return tailSilenceS, tailDecayS, nil
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
