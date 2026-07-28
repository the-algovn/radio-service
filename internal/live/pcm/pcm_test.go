package pcm

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func s16(vals ...int16) []byte {
	b := make([]byte, len(vals)*SampleBytes)
	for i, v := range vals {
		binary.LittleEndian.PutUint16(b[i*SampleBytes:], uint16(v))
	}
	return b
}

func read16(b []byte) []int16 {
	out := make([]int16, len(b)/SampleBytes)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*SampleBytes:]))
	}
	return out
}

func TestLinearGainsAlwaysSumToOne(t *testing.T) {
	for i := range 101 {
		p := float64(i) / 100
		gOut, gIn := Linear(p)
		require.InDelta(t, 1.0, float64(gOut+gIn), 1e-6, "p=%v", p)
	}
}

func TestLinearClampsOutOfRange(t *testing.T) {
	gOut, gIn := Linear(-0.5)
	require.Equal(t, float32(1), gOut)
	require.Equal(t, float32(0), gIn)

	gOut, gIn = Linear(1.5)
	require.Equal(t, float32(0), gOut)
	require.Equal(t, float32(1), gIn)
}

func TestScaleRampInterpolatesAcrossTheBuffer(t *testing.T) {
	buf := s16(1000, 1000, 1000, 1000)
	ScaleRamp(buf, 0, 1)
	require.Equal(t, []int16{0, 333, 667, 1000}, read16(buf))
}

func TestScaleRampConstantGain(t *testing.T) {
	buf := s16(100, -100)
	ScaleRamp(buf, 0.5, 0.5)
	require.Equal(t, []int16{50, -50}, read16(buf))
}

func TestScaleRampSingleSampleUsesStartGain(t *testing.T) {
	buf := s16(1000)
	ScaleRamp(buf, 0.25, 1)
	require.Equal(t, []int16{250}, read16(buf))
}

func TestScaleRampLeavesOddTrailingByte(t *testing.T) {
	buf := append(s16(1000), 0x7f)
	ScaleRamp(buf, 0, 0)
	require.Equal(t, byte(0x7f), buf[len(buf)-1])
}

// The whole reason saturation is mandatory: a wrapping int16 conversion
// flips polarity, which is exactly the click this feature exists to remove.
func TestAddSatNeverWrapsPolarity(t *testing.T) {
	dst := s16(30000)
	AddSat(dst, s16(30000))
	require.Equal(t, []int16{32767}, read16(dst))

	dst = s16(-30000)
	AddSat(dst, s16(-30000))
	require.Equal(t, []int16{-32768}, read16(dst))
}

func TestAddSatAddsSampleWise(t *testing.T) {
	dst := s16(100, -100, 0)
	AddSat(dst, s16(20, 20, -5))
	require.Equal(t, []int16{120, -80, -5}, read16(dst))
}

func TestAddSatIgnoresExtraSamples(t *testing.T) {
	dst := s16(100)
	AddSat(dst, s16(1, 2, 3))
	require.Equal(t, []int16{101}, read16(dst))
}

// The property that makes linear crossfade clip-free by construction:
// |gOut*a + gIn*b| <= max(|a|,|b|) for gOut+gIn == 1.
func TestLinearCrossfadeNeverExceedsInputPeak(t *testing.T) {
	for _, pair := range [][2]int16{{32767, 32767}, {-32768, 32767}, {32767, -32768}, {-32768, -32768}} {
		for i := range 101 {
			p := float64(i) / 100
			gOut, gIn := Linear(p)

			a := s16(pair[0])
			b := s16(pair[1])
			ScaleRamp(a, gOut, gOut)
			ScaleRamp(b, gIn, gIn)
			AddSat(a, b)

			peak := math.Max(math.Abs(float64(pair[0])), math.Abs(float64(pair[1])))
			require.LessOrEqual(t, math.Abs(float64(read16(a)[0])), peak+1,
				"pair=%v p=%v", pair, p)
		}
	}
}
