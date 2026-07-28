package programmer

import "context"

// librarySample is how many library rows the brief shows per decision. The
// window rotates rather than sampling randomly, so the DJ sees the whole shelf
// over time and tests stay reproducible.
const librarySample = 25

// BriefTrack is a library row (or the next-up track) shown to the model.
type BriefTrack struct {
	YTID    string `json:"yt_id"`
	Title   string `json:"title"`
	Channel string `json:"channel"`
}

// BriefPlay is one recent air-log entry. Source, Reason and MinutesAgo let the
// DJ build an arc instead of guessing; all three come straight off live.Entry.
type BriefPlay struct {
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Source     string `json:"source"` // ai | listener | shuffle
	Reason     string `json:"reason"`
	MinutesAgo int    `json:"minutes_ago"`
}

// BriefPend is one pending queue item — BOTH listener requests and the
// programmer's own AI picks, so the DJ can see what it already queued.
type BriefPend struct {
	Title  string `json:"title"`
	Source string `json:"source"` // ai | listener
	Reason string `json:"reason"`
}

// BriefLibrary is the shelf: its true size plus the current rotating window.
type BriefLibrary struct {
	Total  int64        `json:"total"`
	Sample []BriefTrack `json:"sample"`
}

// Brief is the delimited data block the model programs from.
type Brief struct {
	LocalTime   string       `json:"local_time"`
	Listeners   int          `json:"listeners"`
	NextUp      *BriefTrack  `json:"next_up,omitempty"`
	RecentPlays []BriefPlay  `json:"recent_plays"`
	Pending     []BriefPend  `json:"pending"`
	Library     BriefLibrary `json:"library"`
}

// buildBrief assembles the model's data block: station-local time, room state,
// recent plays with their provenance, the whole pending queue, and the current
// rotating library window. It advances the library cursor as a side effect, so
// consecutive decisions see different shelves.
func (p *Programmer) buildBrief(ctx context.Context) (Brief, error) {
	now := p.d.Clock.Now().In(p.d.Location)
	b := Brief{LocalTime: now.Format("Monday 15:04")}

	if n, err := p.d.Listeners.Count(ctx); err == nil {
		b.Listeners = n
	}

	if nu, found, err := p.d.Sched.GetNextUp(ctx); err == nil && found {
		b.NextUp = &BriefTrack{YTID: nu.YTID, Title: nu.Title, Channel: nu.Channel}
	}

	plays, err := p.d.Log.History(ctx, briefPlays)
	if err != nil {
		return Brief{}, err
	}
	for _, e := range plays {
		mins := int(now.Sub(e.StartedAt.In(p.d.Location)).Minutes())
		if mins < 0 {
			mins = 0
		}
		b.RecentPlays = append(b.RecentPlays, BriefPlay{
			Title: e.Title, Artist: e.Artist, Source: e.Source,
			Reason: e.Reason, MinutesAgo: mins,
		})
	}

	pending, err := p.d.Requests.Pending(ctx)
	if err != nil {
		return Brief{}, err
	}
	for _, it := range pending {
		b.Pending = append(b.Pending, BriefPend{Title: it.Title, Source: it.Source, Reason: it.Reason})
	}

	total, err := p.d.Library.Count(ctx, "")
	if err != nil {
		return Brief{}, err
	}
	b.Library.Total = total
	if total > 0 {
		if p.cursor >= int(total) {
			p.cursor = 0
		}
		tracks, err := p.d.Library.List(ctx, "", librarySample, p.cursor)
		if err != nil {
			return Brief{}, err
		}
		for _, tr := range tracks {
			b.Library.Sample = append(b.Library.Sample, BriefTrack{YTID: tr.YTID, Title: tr.Title, Channel: tr.Channel})
		}
		p.cursor += librarySample
	}
	return b, nil
}
