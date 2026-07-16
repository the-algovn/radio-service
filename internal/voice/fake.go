package voice

import (
	"context"
	"encoding/binary"
)

// Fake returns 1s of 8kHz silence as a valid WAV — keyless dev mode.
type Fake struct{}

func (Fake) Synthesize(_ context.Context, _, _ string, _ float64) ([]byte, string, error) {
	const sampleRate, seconds = 8000, 1
	n := sampleRate * seconds * 2 // 16-bit mono
	buf := make([]byte, 44+n)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+n))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(buf[22:24], 1) // mono
	binary.LittleEndian.PutUint32(buf[24:28], sampleRate)
	binary.LittleEndian.PutUint32(buf[28:32], sampleRate*2)
	binary.LittleEndian.PutUint16(buf[32:34], 2)
	binary.LittleEndian.PutUint16(buf[34:36], 16)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(n))
	return buf, "wav", nil
}
