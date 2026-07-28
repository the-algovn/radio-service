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
	Cached    bool   `json:"-"`      // library hit → enqueue ready, not approved
}

// resolve turns phase-1 intent into a pool of real tracks, with no LLM
// involvement. Resolution order is respins, then the library query, then ranked
// search results, so explicitly named intent is never crowded out by the cap.
//
// Individual source failures are logged and skipped rather than failing the
// decision; only a filter-store read error is fatal.
func (p *Programmer) resolve(ctx context.Context, in Intent) ([]Candidate, error) {
	recent, err := p.d.Log.RecentYTIDs(ctx, recentWindow)
	if err != nil {
		return nil, err
	}
	recentSet := make(map[string]bool, len(recent))
	for _, id := range recent {
		recentSet[id] = true
	}

	var pool []Candidate
	seen := map[string]bool{}

	add := func(c Candidate) bool {
		if len(pool) >= poolCap || seen[c.YTID] {
			return false
		}
		skip, err := p.filtered(ctx, c.YTID, c.DurationS, recentSet)
		if err != nil {
			p.d.Logger.Error("programmer: filter read failed", "yt_id", c.YTID, "err", err)
			return false
		}
		if skip {
			return false
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
		return true
	}

	// 1. Respins the model named explicitly.
	for _, id := range in.Respins {
		tr, ok, err := p.d.Library.Get(ctx, id)
		if err != nil || !ok {
			continue
		}
		add(Candidate{
			YTID: tr.YTID, Title: tr.Title, Channel: tr.Channel,
			DurationS: int64(tr.DurationS), Source: sourceLibrary, Cached: true,
		})
	}

	// 2. The library shelf, by the model's search term.
	if in.LibraryQuery != "" {
		trs, err := p.d.Library.List(ctx, in.LibraryQuery, poolCap, 0)
		if err != nil {
			p.d.Logger.Error("programmer: library list failed", "query", in.LibraryQuery, "err", err)
		}
		for _, tr := range trs {
			add(Candidate{
				YTID: tr.YTID, Title: tr.Title, Channel: tr.Channel,
				DurationS: int64(tr.DurationS), Source: sourceLibrary, Cached: true,
			})
		}
	}

	// 3. YouTube discovery, in ranked order.
	for _, q := range in.Searches {
		cs, err := p.d.Search.Search(ctx, q, searchN)
		if err != nil {
			p.d.Logger.Error("programmer: search failed", "query", q, "err", err)
			continue
		}
		for _, sc := range ingest.Rank(q, cs) {
			_, cached, _ := p.d.Library.Get(ctx, sc.YTID)
			add(Candidate{
				YTID: sc.YTID, Title: sc.Title, Channel: sc.Channel,
				DurationS: sc.DurationS, Source: sourceYouTube, Cached: cached,
			})
		}
	}
	return pool, nil
}
