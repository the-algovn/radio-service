package programmer

import (
	"context"

	"github.com/the-algovn/radio-service/internal/ingest"
)

// dropReason names why a candidate did not reach the pool. It replaces a bare
// bool so a decision can report a histogram instead of a single count — the
// difference between "candidates=0" and knowing which predicate ate all of them.
type dropReason string

const (
	dropNone      dropReason = ""
	dropTooShort  dropReason = "too-short"
	dropTooLong   dropReason = "too-long"
	dropLive      dropReason = "live"
	dropShortForm dropReason = "short-form"
	dropRecent    dropReason = "recent"
	dropQueued    dropReason = "queued"
	dropDupe      dropReason = "dupe"
	dropPoolFull  dropReason = "pool-full"
)

// factsOf is everything classify needs about a candidate, independent of where
// it came from. Library tracks always have a known duration and are never live
// or short-form.
type factsOf struct {
	YTID          string
	DurationS     int64
	DurationKnown bool
	Live          bool
	ShortForm     bool
}

func factsFrom(c ingest.Candidate) factsOf {
	return factsOf{
		YTID: c.YTID, DurationS: c.DurationS, DurationKnown: c.DurationKnown,
		Live: c.Live, ShortForm: c.ShortForm,
	}
}

// factsOfTrack builds facts for a library row, whose duration is always known
// (it was ffprobed at acquire time).
func factsOfTrack(ytID string, durationS int64) factsOf {
	return factsOf{YTID: ytID, DurationS: durationS, DurationKnown: true}
}

// guards is the per-decision rejection state, read ONCE. Previously each
// candidate hit the database on its own, which cost up to 34 round-trips per
// decision and made a transient DB error silently thin the pool.
type guards struct {
	recent map[string]bool
}

func (p *Programmer) buildGuards(ctx context.Context) (guards, error) {
	recent, err := p.d.Log.RecentYTIDs(ctx, recentWindow)
	if err != nil {
		return guards{}, err
	}
	set := make(map[string]bool, len(recent))
	for _, id := range recent {
		set[id] = true
	}
	return guards{recent: set}, nil
}

// classify reports why a candidate must be skipped, or dropNone to keep it.
//
// Order matters: the most specific disqualification wins, so a live stream is
// reported as "live" rather than as the "too-short" it used to be mistaken for.
func (p *Programmer) classify(ctx context.Context, f factsOf, g guards) (dropReason, error) {
	switch {
	case f.Live:
		return dropLive, nil
	case f.ShortForm:
		return dropShortForm, nil
	}
	// An unknown duration is admitted on purpose. acquire re-probes with ffprobe
	// and enforces MaxDurationS before Library.Add, so a wrong guess here costs
	// pool quality; rejecting would cost pool emptiness, which is the bug.
	if f.DurationKnown {
		switch {
		case f.DurationS < minTrackSeconds:
			return dropTooShort, nil
		case f.DurationS > maxTrackSeconds:
			return dropTooLong, nil
		}
	}
	if g.recent[f.YTID] {
		return dropRecent, nil
	}
	queued, err := p.d.Requests.HasPendingYTID(ctx, f.YTID)
	if err != nil {
		return dropQueued, err
	}
	if queued {
		return dropQueued, nil
	}
	return dropNone, nil
}
