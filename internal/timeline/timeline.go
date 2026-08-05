// Package timeline projects the station's running order.
//
// It is PURE: no database, no clock of its own, no I/O. Everything it needs
// arrives on State, which the gRPC handler assembles in one read pass. That
// is what makes the whole projection table-testable without fixtures, and it
// is also what keeps the projection off the audio path.
//
// The package's one hard obligation is that its DEPTH-1 music choice matches
// what internal/live's planNext would choose at the same moment. If they
// drift, the console lies with a straight face and nothing else notices —
// which is why TestProjectMatchesPeekNext exists.
package timeline

import (
	"time"

	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/schedule"
	"github.com/the-algovn/radio-service/internal/station"
)

// Segment kinds — the WIRE vocabulary, which is not the stored vocabulary.
//
//	stored (talk_segment.kind)   wire (here)
//	"seam"                    -> KindDJ        "dj"      <- a deliberate RENAME
//	"station_id"              -> KindStationID "station_id"  <- same value
//	(air_log rows)            -> KindTrack     "track"
//	(synthesised by the walk) -> KindUnknown   "unknown"
//
// So KindStationID mirrors showlog.KindStationID / live.ClipStationID by value,
// but KindDJ deliberately does NOT mirror showlog.KindSeam — "seam" names the
// boundary a clip is inserted at, which is engine vocabulary a console reader
// has no use for. The translation happens at the single read site.
//
// Redeclared rather than imported so this package depends on neither live nor
// showlog.
const (
	KindTrack     = "track"
	KindDJ        = "dj"
	KindStationID = "station_id"
	KindUnknown   = "unknown"
)

// KindFromEngine translates an engine/stored talk kind (live.ClipSeam,
// live.ClipStationID — same values as showlog.KindSeam/KindStationID) to the
// wire vocabulary. It is the ONE translator: both read sites, the past path in
// radioserver and the prepared-clip arm in the forward walk, go through here,
// so "seam" can never reach a console.
func KindFromEngine(kind string) string {
	switch kind {
	case "seam":
		return KindDJ
	case KindStationID:
		return KindStationID
	default:
		return KindUnknown
	}
}

// Certainty is the honesty contract. Only Aired and Airing are facts;
// everything else is a claim of decreasing strength. Prepared is deliberately
// NOT above Projected: a prepared clip can still evaporate at Take if its
// anchor has drifted.
const (
	CertaintyAired     = "aired"
	CertaintyAiring    = "airing"
	CertaintyCommitted = "committed"
	CertaintyPrepared  = "prepared"
	CertaintyProjected = "projected"
	CertaintyDue       = "due"
	CertaintyUnknown   = "unknown"
	CertaintyStaging   = "staging"
)

// Gate values explain why no break is coming, named for the thing to fix.
const (
	GateOK          = "ok"
	GateOffAir      = "off_air"
	GateDJDisabled  = "dj_disabled"
	GateAIPaused    = "ai_paused"
	GateBudget      = "budget_reached"
	GateNoListeners = "no_listeners"
)

const (
	// HorizonS bounds the forward walk. Unknown-shuffle blocks are otherwise
	// unbounded, since the walk can always emit one more.
	HorizonS = 30 * 60
	// MaxSegments is the hard cap, belt to HorizonS' braces.
	MaxSegments = 24
	// FallbackMedianS sizes unknown blocks when the air log is too thin to
	// have a meaningful median (a fresh install, or a station that has never
	// aired). 3:30 is a middling pop song.
	FallbackMedianS = 210
	// AnchorTolerance mirrors live's anchorFreshTolerance. A prepared clip
	// whose anchor has drifted further than this is discarded AT TAKE, so
	// reporting it as `prepared` would promise a break that cannot air.
	AnchorTolerance = time.Second
)

// Segment is one item of the timeline, past or future. Fields that do not
// apply to a row are zero — music rows carry no Script, talk rows no YTID.
// Consumers must branch on Kind, never assume population.
type Segment struct {
	SegmentID string
	Kind      string
	Certainty string

	Title, Artist, ThumbnailURL string
	// YTID is present on music rows only. The forward walk needs it to run
	// the same anchor-freshness test Take runs against a prepared clip.
	YTID      string
	StartedAt time.Time
	DurationS int

	// request/air provenance
	Source, RequestedByName, Reason string
	RequestID, Status               string

	// talk provenance
	Script, BacksellTitle, PromiseTitle, CorrelationID string
	Model                                              string
	InTokens, OutTokens, LatencyMS                     int
	CostUSD                                            float64
}

// DirectorSnapshot is a value copy of the director's cadence state.
//
// Present distinguishes "the director exists and has nothing prepared" from
// "there is no director at all" (RADIO_DJ_ENABLED unset). Conflating them
// would tell an operator to un-pause something that is not paused.
//
// It carries NO cadence SETTINGS: BreakEvery and StationIDMin live on the
// station row and are re-read every tick, so they arrive via State.Station.
type DirectorSnapshot struct {
	Present bool

	HasClip             bool
	ClipKind            string
	ClipDurationS       float64
	ClipAnchorYTID      string
	ClipAnchorStartedAt time.Time
	ClipBacksellTitle   string
	ClipPromiseTitle    string
	ClipCorrelationID   string

	FinishedSinceSeam int
	LastStationID     time.Time

	// StationIDsAvailable is a term of the engine's due test: with no usable
	// station-ID lines the engine falls through to BreakEvery and airs a seam
	// where the projector would otherwise promise a station_id.
	StationIDsAvailable bool
}

// State is everything Project needs, gathered once by the caller.
type State struct {
	Now     time.Time
	Station station.Station

	// Airing is the newest stored row still running; nil between items or off
	// air. It MAY be a talk segment — the walk must handle that.
	Airing *Segment

	// NextUp is the committed pin, or nil. NOTE the PG convention: an empty
	// YTID means NOT committed (ClearNextUp writes empty strings and the
	// singleton row always exists). The caller resolves that before setting
	// this field.
	NextUp *schedule.NextUp

	// Pending is the whole pending set (approved AND ready) in the store's
	// documented order. Only `ready` rows hold an air slot.
	Pending []request.Item

	// Durations maps ytID -> duration_s for tracks the caller resolved from the
	// library. An entry exists for every resolvable track: the pin's track,
	// ready-request tracks, and the airing track for anchor checking.
	//
	// Absent means the track is NOT AIRABLE, never "unknown length." A
	// fallback would render a phantom track as committed, which is how four
	// implementation divergences were found before this comment existed.
	// The pin arm REJECTS a NextUp whose YTID is missing from this map; the
	// ready-queue arm pops heads until it finds one that IS present.
	Durations map[string]int

	// MedianTrackS sizes unknown-shuffle blocks; 0 means use FallbackMedianS.
	MedianTrackS int

	Dir DirectorSnapshot

	Listeners           int
	SpentUSD, BudgetUSD float64
}
