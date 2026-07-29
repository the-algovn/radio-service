package programmer

import (
	"context"
	"strings"

	"github.com/the-algovn/radio-service/internal/ingest"
)

const (
	// poolCap bounds the candidate list shown to phase 2, which bounds its
	// input tokens and so the per-decision cost.
	poolCap = 12

	// libraryQueryCap bounds what the library query may contribute, so a shelf
	// that happens to match cannot consume the whole pool and starve discovery.
	// Respins are exempt — they are explicitly named intent, already bounded at
	// maxRespins. With a small library, discovery is the scarce resource; if the
	// shelf ever grows large enough for that to be backwards, this is the knob.
	libraryQueryCap = poolCap / 3

	sourceYouTube = "youtube"
	sourceLibrary = "library"
)

// Candidate is one real, already-filtered track phase 2 may choose from.
type Candidate struct {
	YTID      string `json:"yt_id"`
	Title     string `json:"title"`
	Channel   string `json:"channel"`
	DurationS int64  `json:"duration_s"`
	Source    string `json:"source"` // youtube | library
	// Score and Notes are the ranker's verdict, carried through so phase 2 can
	// see that a candidate is a compilation or a karaoke version. They were
	// computed and discarded before, which left the model choosing on title
	// alone. Library candidates are unranked and carry neither.
	Score int      `json:"score,omitempty"`
	Notes []string `json:"notes,omitempty"`
	// ThumbnailURL rides to the queue, not to the model — hence json:"-", which
	// also keeps an attacker-supplied URL out of the phase-2 prompt.
	ThumbnailURL string `json:"-"`
	Cached       bool   `json:"-"` // library hit → enqueue ready, not approved
}

// resolve turns phase-1 intent into a pool of real tracks, with no LLM
// involvement. Resolution order is respins, then the library query, then ranked
// search results, so explicitly named intent is never crowded out by the cap.
//
// Individual source failures are logged and skipped rather than failing the
// decision; only a guard read error is fatal.
func (p *Programmer) resolve(ctx context.Context, in Intent) ([]Candidate, error) {
	g, err := p.buildGuards(ctx)
	if err != nil {
		return nil, err
	}

	var pool []Candidate
	seen := map[string]bool{}
	drops := map[dropReason]int{}
	rawSearch := 0

	add := func(c Candidate, f factsOf) {
		if len(pool) >= poolCap {
			drops[dropPoolFull]++
			return
		}
		if seen[c.YTID] {
			drops[dropDupe]++
			return
		}
		why, err := p.classify(ctx, f, g)
		if err != nil {
			p.d.Logger.ErrorContext(ctx, "programmer: filter read failed", "yt_id", c.YTID, "err", err)
			drops[why]++
			return
		}
		if why != dropNone {
			drops[why]++
			return
		}
		seen[c.YTID] = true
		// Title/Channel are attacker-influenceable: anyone can title a YouTube
		// video, and this text rides straight into the phase-2 prompt inside
		// the <candidates> block. Stripping '<'/'>' here is what actually
		// protects that delimiter — it must not depend on json.Marshal's
		// default HTML-escaping of '<'/'>', which a switch to
		// json.Encoder.SetEscapeHTML(false) would silently turn off.
		c.Title = strings.NewReplacer("<", "", ">", "").Replace(c.Title)
		c.Channel = strings.NewReplacer("<", "", ">", "").Replace(c.Channel)
		pool = append(pool, c)
	}

	// 1. Respins the model named explicitly.
	for _, id := range in.Respins {
		tr, ok, err := p.d.Library.Get(ctx, id)
		if err != nil || !ok {
			// Silent until now. The brief shows the model library.sample WITH
			// yt_ids but recent_plays/pending with titles only, so it cannot
			// tell which ids are burned and keeps re-proposing them.
			p.d.Logger.InfoContext(ctx, "programmer: respin not in library", "yt_id", id)
			continue
		}
		add(Candidate{
			YTID: tr.YTID, Title: tr.Title, Channel: tr.Channel,
			DurationS: int64(tr.DurationS), Source: sourceLibrary, Cached: true,
		}, factsOfTrack(tr.YTID, int64(tr.DurationS)))
	}

	// 2. The library shelf, by the model's search term.
	if in.LibraryQuery != "" {
		trs, err := p.d.Library.List(ctx, in.LibraryQuery, poolCap, 0)
		if err != nil {
			p.d.Logger.ErrorContext(ctx, "programmer: library list failed", "query", in.LibraryQuery, "err", err)
		}
		fromLibrary := 0
		for _, tr := range trs {
			if fromLibrary >= libraryQueryCap {
				break
			}
			before := len(pool)
			add(Candidate{
				YTID: tr.YTID, Title: tr.Title, Channel: tr.Channel,
				DurationS: int64(tr.DurationS), Source: sourceLibrary, Cached: true,
			}, factsOfTrack(tr.YTID, int64(tr.DurationS)))
			if len(pool) > before {
				fromLibrary++
			}
		}
	}

	// 3. YouTube discovery. Every query's results are gathered first and ranked
	// ONCE, so a strong result from the last query is not crowded out by a weak
	// one from the first — which is what per-query ranking used to do.
	var found []ingest.Candidate
	for _, q := range in.Searches {
		cs, err := p.d.Search.Search(ctx, q, searchN)
		if err != nil {
			p.d.Logger.ErrorContext(ctx, "programmer: search failed", "query", q, "err", err)
			continue
		}
		rawSearch += len(cs)
		found = append(found, cs...)
	}
	for _, sc := range ingest.Rank(strings.Join(in.Searches, " "), found) {
		if len(pool) >= poolCap {
			break
		}
		_, cached, _ := p.d.Library.Get(ctx, sc.YTID)
		add(Candidate{
			YTID: sc.YTID, Title: sc.Title, Channel: sc.Channel,
			DurationS: sc.DurationS, Source: sourceYouTube, Cached: cached,
			Score: sc.Score, Notes: sc.Notes, ThumbnailURL: sc.ThumbnailURL,
		}, factsFrom(sc.Candidate))
	}

	p.logFunnel(ctx, rawSearch, len(pool), drops)
	return pool, nil
}

// logFunnel emits the one line that makes a 34-candidates-in, 0-out collapse
// diagnosable. Without it, "candidates=0" is indistinguishable between the
// duration filter eating everything, recency eating everything, and yt-dlp
// returning no rows at all — and a search returning zero entries is not an
// error, so it logs nothing on its own.
func (p *Programmer) logFunnel(ctx context.Context, rawSearch, pooled int, drops map[dropReason]int) {
	attrs := []any{"raw_search", rawSearch, "pooled", pooled}
	for _, r := range []dropReason{
		dropTooShort, dropTooLong, dropLive, dropShortForm,
		dropRecent, dropNextUp, dropFailed, dropQueued, dropDupe, dropPoolFull, dropReadFailed,
	} {
		if n := drops[r]; n > 0 {
			attrs = append(attrs, string(r), n)
		}
	}
	p.d.Logger.InfoContext(ctx, "programmer: funnel", attrs...)
}
