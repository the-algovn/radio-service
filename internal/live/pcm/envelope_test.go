package pcm

import (
	"bytes"
	"encoding/binary"
	"math"
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
