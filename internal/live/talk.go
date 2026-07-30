package live

import "time"

// Clip kinds (v2 talk breaks). ClipSeam closes the song that just played and
// opens the next one in a single break (spec 2026-07-29-radio-dj-seam-breaks);
// it replaced ClipBacksell, which only ever looked backwards.
const (
	ClipSeam      = "seam"
	ClipStationID = "station_id"
)

// Clip is one pre-rendered talk break: a raw s16le/48kHz/stereo PCM file on
// local disk, prepared ahead of air by the director. AnchorYTID +
// AnchorStartedAt identify the air-log entry the clip was written about
// (seam freshness); both are zero on a station_id, which is always fresh.
type Clip struct {
	Path            string
	DurationS       float64 // exact, from byte size — pacing truth
	Script          string  // logs only, never published
	Kind            string  // ClipSeam | ClipStationID
	AnchorYTID      string
	AnchorStartedAt time.Time
}

// TalkSource hands finished talk clips to the feeder. Implemented by
// internal/director; nil = feature absent (feeder byte-for-byte unchanged).
// Take must never block — ok=false when nothing is ready — and owns
// staleness: a stale seam is deleted and the slot cleared inside Take
// before returning ok=false. TrackFinished is the format-clock signal:
// exactly once per air-logged MUSIC item when it stops airing (EOF, operator
// skip, or crash-cap skip); never for talk clips or failed-open tracks.
type TalkSource interface {
	TrackFinished(e Entry)
	Take(justFinished Entry) (Clip, bool)
}
