package pcm

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
)

// tone renders n seconds of a constant-amplitude signal at 48kHz stereo,
// scaled by gain(t) where t runs 0..1 across the whole buffer.
func tone(seconds float64, gain func(t float64) float64) []byte {
	frames := int(seconds * 48000)
	b := make([]byte, frames*2*SampleBytes)
	for i := range frames {
		t := 0.0
		if frames > 1 {
			t = float64(i) / float64(frames-1)
		}
		// A square-ish alternating sample keeps RMS equal to amplitude,
		// so the tests can reason in absolute amplitude rather than about
		// the shape of a sine.
		amp := 12000 * gain(t)
		v := int16(amp)
		if i%2 == 1 {
			v = int16(-amp)
		}
		off := i * 2 * SampleBytes
		binary.LittleEndian.PutUint16(b[off:], uint16(v))
		binary.LittleEndian.PutUint16(b[off+SampleBytes:], uint16(v))
	}
	return b
}

func flat(float64) float64 { return 1 }

// jitterWindows perturbs alternating windows of buf by lsb raw amplitude
// units. tone() produces bit-identical consecutive windows for a constant
// gain, which real audio never does — even a held pad or room-tone tail has
// some dither. Alternating the perturbation guarantees no two adjacent
// windows are ever bit-identical, matching that reality.
func jitterWindows(buf []byte, windowBytes int, lsb int16) []byte {
	out := append([]byte(nil), buf...)
	on := true
	for off := 0; off+windowBytes <= len(out); off += windowBytes {
		if on {
			for i := off; i+SampleBytes <= off+windowBytes; i += SampleBytes {
				v := int16(binary.LittleEndian.Uint16(out[i:]))
				binary.LittleEndian.PutUint16(out[i:], uint16(v+lsb))
			}
		}
		on = !on
	}
	return out
}

// jitterPatternDB is a fixed pseudo-random sequence of per-window dB offsets
// with roughly 2-4dB of swing — the magnitude measured window-to-window on
// real production audio, not the hairline fractions of a dB
// TestTailCuesToleratesJitteryLeadIn uses. It is a lightly filtered random
// walk, not independent noise per window: real audio's RMS jitter carries
// over between adjacent 50ms windows because it comes from the same
// continuous signal, and pure per-window-independent noise turns out to be
// a harsher, less realistic adversary than what real tracks measured as —
// it can still alias against a small median window and defeat the fix for
// reasons that don't occur in practice. Seeded, so the test is reproducible.
var jitterPatternDB = func() []float64 {
	rng := rand.New(rand.NewSource(1))
	const alpha = 0.5 // adjacent-window correlation
	pattern := make([]float64, 512)
	prev := 0.0
	for i := range pattern {
		noise := -4 + rng.Float64()*8 // i.i.d. component, dB
		prev = alpha*prev + (1-alpha)*noise
		pattern[i] = prev
	}
	return pattern
}()

// jitteredDecline renders seconds of audio whose level follows a linear dB
// ramp from 0 down to floorDB, with jitterPatternDB riding on top of every
// window. floorDB=0 renders jitter with no underlying trend at all.
func jitteredDecline(seconds, floorDB float64) []byte {
	windowFrames := 48000 * windowMS / 1000
	frames := int(seconds * 48000)
	numWindows := frames / windowFrames
	b := make([]byte, frames*2*SampleBytes)
	for w := range numWindows {
		trendDB := 0.0
		if numWindows > 1 {
			trendDB = floorDB * float64(w) / float64(numWindows-1)
		}
		amp := 12000 * math.Pow(10, (trendDB+jitterPatternDB[w%len(jitterPatternDB)])/20)
		v := int16(amp)
		for i := w * windowFrames; i < (w+1)*windowFrames; i++ {
			sv := v
			if i%2 == 1 {
				sv = -v
			}
			off := i * 2 * SampleBytes
			binary.LittleEndian.PutUint16(b[off:], uint16(sv))
			binary.LittleEndian.PutUint16(b[off+SampleBytes:], uint16(sv))
		}
	}
	return b
}

func TestTailCuesColdEndingHasNoSilenceAndNoDecay(t *testing.T) {
	// 3s at full level, stopping dead. Both cues must be ~0 — a cold
	// ending has no room to overlap.
	sil, dec, err := TailCues(bytes.NewReader(tone(3, flat)), 48000, 2)
	require.NoError(t, err)
	require.InDelta(t, 0.0, sil, 0.05)
	require.InDelta(t, 0.0, dec, 0.05)
}

func TestTailCuesMeasuresTrailingSilence(t *testing.T) {
	// 2s of tone then 1s of digital silence.
	buf := append(tone(2, flat), tone(1, func(float64) float64 { return 0 })...)
	sil, _, err := TailCues(bytes.NewReader(buf), 48000, 2)
	require.NoError(t, err)
	require.InDelta(t, 1.0, sil, 0.1)
}

func TestTailCuesMeasuresFadeOutAsDecay(t *testing.T) {
	// 2s flat, then a 2s linear fade to zero. The fade is decay, not
	// silence — only its very tail crosses the floor.
	buf := append(tone(2, flat), tone(2, func(t float64) float64 { return 1 - t })...)
	sil, dec, err := TailCues(bytes.NewReader(buf), 48000, 2)
	require.NoError(t, err)
	require.Greater(t, dec, 1.0, "a 2s fade should register well over a second of decay")
	require.Less(t, sil+dec, 2.6, "silence and decay must not double-count the same tail")
}

// The regression that motivates measuring decay from the start of silence
// rather than from EOF: with a fade AND trailing silence, the two runs must
// partition the tail, never overlap it.
func TestTailCuesSilenceAndDecayAreDisjoint(t *testing.T) {
	buf := tone(2, flat)
	buf = append(buf, tone(2, func(t float64) float64 { return 1 - t })...)
	buf = append(buf, tone(1, func(float64) float64 { return 0 })...)

	sil, dec, err := TailCues(bytes.NewReader(buf), 48000, 2)
	require.NoError(t, err)
	require.InDelta(t, 1.0, sil, 0.15, "one second of digital silence")
	require.Greater(t, dec, 0.8, "the fade must still be seen behind the silence")
	require.Less(t, sil+dec, 3.3, "the sum must not exceed the real 3s tail")
}

// A lead-in that is merely close to flat, not bit-exact, is what real audio
// actually looks like: a held pad, a reverb tail, room tone before the fade
// starts. The decay boundary must not be fooled by that near-flatness into
// swallowing the whole lead-in the way it would be fooled by exact equality.
func TestTailCuesToleratesJitteryLeadIn(t *testing.T) {
	windowBytes := 48000 * windowMS / 1000 * 2 * SampleBytes
	leadIn := jitterWindows(tone(2, flat), windowBytes, 3)
	buf := append(leadIn, tone(2, func(t float64) float64 { return 1 - t })...)
	sil, dec, err := TailCues(bytes.NewReader(buf), 48000, 2)
	require.NoError(t, err)
	require.Less(t, sil+dec, 2.6, "a jittery lead-in must not be swept into the decay run")
}

// Pins minDecayDropDB's boundary from both sides: a decline too small to be
// a real fade must report zero decay, and one clearly past the floor must
// not.
func TestTailCuesGatesOnMinimumDrop(t *testing.T) {
	rampTo := func(targetDB float64) []byte {
		target := math.Pow(10, targetDB/20)
		return append(tone(2, flat), tone(1, func(t float64) float64 { return 1 - t*(1-target) })...)
	}

	_, dec, err := TailCues(bytes.NewReader(rampTo(-4)), 48000, 2)
	require.NoError(t, err)
	require.Equal(t, 0.0, dec, "a ~4 dB decline is below the minimum drop and must not register as decay")

	_, dec, err = TailCues(bytes.NewReader(rampTo(-10)), 48000, 2)
	require.NoError(t, err)
	require.Greater(t, dec, 0.0, "a ~10 dB decline is past the minimum drop and must register as decay")
}

// The regression that motivated smoothing the walk: real audio jitters
// several dB between adjacent windows even mid-decline (measured on
// production tracks — see envelope.go), which a plain immediate-predecessor
// comparison cannot survive even with decayTolerance's 1.5dB slack. Without
// smoothing, this fixture's decline is invisible to TailCues.
func TestTailCuesRecoversDecayUnderRealisticJitter(t *testing.T) {
	buf := jitteredDecline(3, -20)
	sil, dec, err := TailCues(bytes.NewReader(buf), 48000, 2)
	require.NoError(t, err)
	require.Equal(t, 0.0, sil, "the decline never reaches the silence floor")
	require.Greater(t, dec, 1.0, "a 3s, 20dB decline must be recovered despite realistic jitter")
}

// The other direction: jitter with no underlying decline must not be read
// as decay either, or the smoothing fix would just always fire regardless
// of whether the track is actually fading.
func TestTailCuesJitteryColdEndingStillHasNoDecay(t *testing.T) {
	buf := jitteredDecline(3, 0) // floorDB=0: jitter only, no trend
	sil, dec, err := TailCues(bytes.NewReader(buf), 48000, 2)
	require.NoError(t, err)
	require.Equal(t, 0.0, sil)
	require.InDelta(t, 0.0, dec, 0.3, "jitter alone, with no real decline, must not read as decay")
}

func TestTailCuesRejectsBadGeometry(t *testing.T) {
	_, _, err := TailCues(bytes.NewReader(nil), 0, 2)
	require.Error(t, err)
	_, _, err = TailCues(bytes.NewReader(nil), 48000, 0)
	require.Error(t, err)
	// sampleRate*windowMS/1000 truncates to 0 below ~20Hz, which used to
	// leave the read loop spinning forever on a zero-length buffer.
	_, _, err = TailCues(bytes.NewReader(nil), 19, 2)
	require.Error(t, err)
}

func TestTailCuesEmptyStreamIsZero(t *testing.T) {
	sil, dec, err := TailCues(bytes.NewReader(nil), 48000, 2)
	require.NoError(t, err)
	require.Equal(t, 0.0, sil)
	require.Equal(t, 0.0, dec)
}

// A stream whose length is not a whole number of frames must not panic or
// mis-index; the ragged tail is simply ignored.
func TestTailCuesToleratesRaggedTail(t *testing.T) {
	buf := append(tone(1, flat), 0x01)
	_, _, err := TailCues(bytes.NewReader(buf), 48000, 2)
	require.NoError(t, err)
}

func TestTailCuesIsSilenceFloorRelative(t *testing.T) {
	// A very quiet but non-silent outro must read as decay, not silence.
	quiet := tone(1, func(float64) float64 { return math.Pow(10, -30.0/20) })
	sil, _, err := TailCues(bytes.NewReader(append(tone(1, flat), quiet...)), 48000, 2)
	require.NoError(t, err)
	require.InDelta(t, 0.0, sil, 0.1, "-30 dBFS is quiet but not silence")
}
