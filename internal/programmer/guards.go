package programmer

import (
	"context"

	"github.com/the-algovn/radio-service/internal/ingest"
	"github.com/the-algovn/radio-service/internal/request"
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
	// dropRecentSong is a DIFFERENT yt_id for the same folded song identity as
	// something recently aired — the duplication every id-keyed guard misses:
	// one song uploaded as an official MV, a "- Topic" track, and a lyric
	// video is three ids that all pass dropRecent.
	dropRecentSong dropReason = "recent-song"
	dropQueued     dropReason = "queued"
	dropDupe       dropReason = "dupe"
	dropPoolFull   dropReason = "pool-full"
	dropNextUp     dropReason = "next-up"
	dropFailed     dropReason = "recently-failed"
	// dropReadFailed is a guard read that errored, NOT a real disqualification.
	// It has its own reason because the funnel histogram is this task's whole
	// point: labelling a Postgres outage as "queued" would misreport the one
	// situation the histogram most needs to name.
	dropReadFailed dropReason = "read-failed"
)

// factsOf is everything classify needs about a candidate, independent of where
// it came from. Library tracks always have a known duration and are never live
// or short-form.
type factsOf struct {
	YTID          string
	SongKey       string
	DurationS     int64
	DurationKnown bool
	Live          bool
	ShortForm     bool
}

// factsFrom builds facts for a YouTube search result. SongKey is left "" —
// a search result carries no artist/track metadata, only a channel name and a
// raw video title, and songkey.Of on that pair mis-keys most Vietnamese
// "Artist - Title" uploads onto the artist alone. That folds every other song
// by the same artist onto one key, so once one airs, dropRecentSong would
// reject the rest of that artist's catalogue for the whole recency window.
// High precision, lower recall: dropRecentSong only fires on tracks whose
// upload carried real Content-ID track/artists metadata (see factsOfTrack).
func factsFrom(c ingest.Candidate) factsOf {
	return factsOf{
		YTID:      c.YTID,
		DurationS: c.DurationS, DurationKnown: c.DurationKnown,
		Live: c.Live, ShortForm: c.ShortForm,
	}
}

// factsOfTrack builds facts for a library row, whose duration is always known
// (it was ffprobed at acquire time). songKey is the row's stored tr.SongKey —
// computed once, at acquire time, from real artist/track metadata — unlike
// factsFrom's best-effort fold of a YouTube search result's channel/title.
func factsOfTrack(ytID string, durationS int64, songKey string) factsOf {
	return factsOf{YTID: ytID, DurationS: durationS, DurationKnown: true, SongKey: songKey}
}

// guards is the per-decision rejection state, read ONCE. Previously each
// candidate hit the database on its own, which cost up to 34 round-trips per
// decision and made a transient DB error silently thin the pool.
type guards struct {
	recent      map[string]bool
	recentSongs map[string]bool
	nextUpID    string
	failed      map[string]bool
}

func (p *Programmer) buildGuards(ctx context.Context) (guards, error) {
	recent, err := p.d.Log.RecentYTIDs(ctx, recentWindow)
	if err != nil {
		return guards{}, err
	}
	g := guards{recent: make(map[string]bool, len(recent)), failed: map[string]bool{}}
	for _, id := range recent {
		g.recent[id] = true
	}
	// Non-fatal: a missing next-up or an unreadable terminal list degrades to
	// today's behaviour rather than skipping the decision entirely. Still
	// logged — Stage 1's whole thesis is that no guard fails silently, and a
	// swallowed error here quietly disables the next-up/failed guards with no
	// trace in the funnel.
	if nu, found, err := p.d.Sched.GetNextUp(ctx); err != nil {
		p.d.Logger.WarnContext(ctx, "programmer: next-up read failed; next-up guard degraded", "err", err)
	} else if found {
		g.nextUpID = nu.YTID
	}
	if items, err := p.d.Requests.RecentTerminal(ctx, failedWindow); err != nil {
		p.d.Logger.WarnContext(ctx, "programmer: recent-terminal read failed; failed guard degraded", "err", err)
	} else {
		for _, it := range items {
			if it.Status == request.StatusFailed {
				g.failed[it.YTID] = true
			}
		}
	}
	// Resolve the recently-aired ids to their song keys, so a DIFFERENT upload
	// of the same song is also blocked. Tracks with no computed key contribute
	// nothing — '' is the not-computed sentinel, not a real identity.
	g.recentSongs = map[string]bool{}
	for id := range g.recent {
		tr, found, err := p.d.Library.Get(ctx, id)
		if err != nil || !found || tr.SongKey == "" {
			continue
		}
		g.recentSongs[tr.SongKey] = true
	}
	return g, nil
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
	// The same song under a different upload defeats every id-keyed guard. An
	// empty key is the not-computed sentinel and must never match, or every
	// uncomputed track would collapse into one.
	if f.SongKey != "" && g.recentSongs[f.SongKey] {
		return dropRecentSong, nil
	}
	// g.nextUpID == "" means no next-up is committed; an empty f.YTID must
	// never match it, or a candidate with no id would be counted as a phantom
	// "next-up" drop in the funnel.
	if g.nextUpID != "" && f.YTID == g.nextUpID {
		return dropNextUp, nil
	}
	if g.failed[f.YTID] {
		return dropFailed, nil
	}
	queued, err := p.d.Requests.HasPendingYTID(ctx, f.YTID)
	if err != nil {
		return dropReadFailed, err
	}
	if queued {
		return dropQueued, nil
	}
	return dropNone, nil
}
