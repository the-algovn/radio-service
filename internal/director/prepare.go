package director

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/the-algovn/radio-service/internal/audit"
	"github.com/the-algovn/radio-service/internal/brain"
	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/persona"
	"github.com/the-algovn/radio-service/internal/schedule"
	"github.com/the-algovn/radio-service/internal/spend"
	"github.com/the-algovn/radio-service/internal/station"
	"github.com/the-algovn/radio-service/internal/talkmem"
)

// normalizeDJ is the last line of defense against hand-edited DB rows; the
// API bounds in radioserver are the authoritative range (spec §4). It only
// catches values that would break synthesis outright.
func normalizeDJ(dj station.DJSettings) station.DJSettings {
	if dj.Rate <= 0 {
		dj.Rate = 1.0
	}
	if dj.MaxChars <= 0 {
		dj.MaxChars = 1024
	}
	return dj
}

// prepare runs one whole clip-preparation attempt: script → TTS → render.
// Every failure is a quiet skip (logged, temp files removed, spend already
// ledgered stays ledgered) — the air never waits on this path.
func (dr *Director) prepare(ctx context.Context, kind string, st station.Station) (live.Clip, bool) {
	dj := normalizeDJ(st.DJ)
	deadline := dr.d.PrepDeadline
	if deadline <= 0 {
		deadline = defaultPrepDeadline
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	sp, known := specFor(kind)
	if !known {
		dr.d.Logger.ErrorContext(ctx, "director: unknown segment kind", "kind", kind)
		return live.Clip{}, false
	}

	var script, anchorYTID string
	var correlationID, backsellTitle, promiseTitle string
	var anchorStartedAt time.Time
	var out brain.Output
	var promised *live.Upcoming
	var just *live.Entry

	if !sp.Generated {
		line, ok := dr.ids.next()
		if !ok {
			return live.Clip{}, false
		}
		script = line
	} else {
		pers, err := persona.Load(dr.d.PersonaDir)
		if err != nil {
			dr.d.Logger.ErrorContext(ctx, "director: persona load failed", "err", err)
			return live.Clip{}, false
		}
		if sp.Anchored {
			entry, found, aerr := dr.d.AirLog.Latest(ctx)
			if aerr != nil || !found {
				if aerr != nil {
					dr.d.Logger.ErrorContext(ctx, "director: air log read failed", "err", aerr)
				}
				return live.Clip{}, false // nothing airing -> nothing to backsell
			}
			just = &entry
			anchorYTID, anchorStartedAt = entry.YTID, entry.StartedAt
			backsellTitle = entry.Title
		}
		// Peek BEFORE generating: the brief needs coming_up. Pin AFTER
		// rendering (below) so a failed preparation never reorders the queue.
		if sp.Promises && dr.d.Peek != nil && dr.d.Sched != nil {
			if up, found, perr := dr.d.Peek(ctx); perr != nil {
				dr.d.Logger.WarnContext(ctx, "director: peek failed; backsell only", "err", perr)
			} else if found {
				promised = &up
				promiseTitle = up.Track.Title
			}
		}
		// Mint BEFORE generateValid - that function rebinds ctx with
		// audit.WithLabel, deriving from whatever it is handed, so an id set
		// afterwards would never reach the audit callback.
		correlationID = newCorrelationID()
		ctx = audit.WithCorrelation(ctx, correlationID)

		rules, ok := brain.RulesFor(kind)
		if !ok {
			dr.d.Logger.ErrorContext(ctx, "director: no rules for kind", "kind", kind)
			return live.Clip{}, false
		}
		from, silent := dr.briefFacts()
		briefJSON, merr := json.Marshal(dr.buildBrief(ctx, st, kind, just, promised, dj.MaxChars, from, silent))
		if merr != nil {
			dr.d.Logger.ErrorContext(ctx, "director: brief marshal failed", "err", merr)
			return live.Clip{}, false
		}
		system, user := brain.BuildScriptPrompts(pers, rules, string(briefJSON))
		out, ok = dr.generateValid(ctx, kind, system, user, dj.MaxChars)
		if !ok {
			return live.Clip{}, false
		}
		script = out.Script
	}

	data, ext, cost, provider, err := dr.d.Voice.Synthesize(ctx, script, dj.VoiceID, dj.Rate)
	if err != nil {
		dr.d.Logger.ErrorContext(ctx, "director: tts failed", "kind", kind, "err", err)
		return live.Clip{}, false
	}
	chars := utf8.RuneCountInString(script)
	if lerr := dr.d.Ledger.Append(ctx, spend.Line{
		TS: time.Now(), Kind: "tts", Provider: provider, Label: "director:" + kind,
		Chars: chars, CostUSD: cost,
	}); lerr != nil {
		dr.d.Logger.ErrorContext(ctx, "director: ledger append failed", "err", lerr)
	}

	n := dr.seq.Add(1)
	takePath := filepath.Join(dr.d.DataDir, fmt.Sprintf("take-%d.%s", n, ext))
	if err := os.WriteFile(takePath, data, 0o644); err != nil {
		dr.d.Logger.ErrorContext(ctx, "director: take write failed", "err", err)
		return live.Clip{}, false
	}
	defer func() { _ = os.Remove(takePath) }()

	outPath := filepath.Join(dr.d.DataDir, fmt.Sprintf("clip-%d.pcm", n))
	durS, err := dr.d.Render(ctx, takePath, outPath)
	if err != nil {
		dr.d.Logger.ErrorContext(ctx, "director: render failed", "kind", kind, "err", err)
		_ = os.Remove(outPath)
		return live.Clip{}, false
	}

	// The pin is the promise, and it goes LAST: everything above can fail, and
	// a pin for a break that never airs would have reordered the queue for
	// nothing. A pin that fails discards the clip — airing an unbacked promise
	// is the single outcome this design exists to prevent.
	if promised != nil && !promised.Committed {
		if perr := dr.d.Sched.SetNextUp(ctx, schedule.NextUp{
			YTID: promised.Track.YTID, Title: promised.Track.Title,
			Channel: promised.Track.Channel, RequestID: promised.RequestID,
		}); perr != nil {
			dr.d.Logger.ErrorContext(ctx, "director: pin failed; discarding clip", "err", perr)
			_ = os.Remove(outPath)
			return live.Clip{}, false
		}
	}

	if sp.Generated && dr.d.TalkMem != nil {
		// Best-effort: the clip is rendered and paid for, so a memory write
		// failure must not lose the break.
		if merr := dr.d.TalkMem.Append(ctx, talkmem.Entry{
			Kind: kind, Summary: out.Summary, Phrases: out.UsedPhrases,
		}); merr != nil {
			dr.d.Logger.ErrorContext(ctx, "director: show memory append failed", "err", merr)
		}
	}
	dr.d.Logger.InfoContext(ctx, "talk clip prepared", "kind", kind, "duration_s", durS, "script", script)
	return live.Clip{Path: outPath, DurationS: durS, Script: script, Kind: kind,
		AnchorYTID: anchorYTID, AnchorStartedAt: anchorStartedAt,
		BacksellTitle: backsellTitle, PromiseTitle: promiseTitle,
		CorrelationID: correlationID}, true
}

// generateValid makes the script call with the on-air validation loop: parse
// failure aborts; validation violations get ONE retry with the violations
// appended. Cost and the spend ledger are handled by the Eino audit callback,
// so this loop no longer prices anything itself.
func (dr *Director) generateValid(ctx context.Context, kind, system, user string, maxChars int) (brain.Output, bool) {
	ctx = audit.WithLabel(ctx, "director:"+kind)
	for attempt := 0; ; attempt++ {
		raw, err := dr.d.Model.Generate(ctx, system, user, brain.ScriptSchema)
		if err != nil {
			dr.d.Logger.ErrorContext(ctx, "director: model call failed", "err", err)
			return brain.Output{}, false
		}
		out, perr := brain.ParseOutput(raw)
		if perr != nil {
			dr.d.Logger.ErrorContext(ctx, "director: parse failed", "err", perr, "raw", string([]rune(raw)[:min(utf8.RuneCountInString(raw), 200)]))
			return brain.Output{}, false
		}
		v := brain.Validate(out.Script, maxChars)
		if len(v) == 0 {
			return out, true
		}
		if attempt > 0 {
			dr.d.Logger.WarnContext(ctx, "director: script invalid after retry; giving up", "violations", v)
			return brain.Output{}, false
		}
		user = user + "\n\nLỗi cần sửa (viết lại toàn bộ):\n- " + strings.Join(v, "\n- ")
	}
}

const (
	tonightCap = 6
	threadCap  = 8
)

// buildBrief assembles the data block for one authored break. just is nil for
// every kind but the seam, and up is nil when nothing could be promised. Every
// read here is best-effort: a failure degrades one FIELD, never the break -
// music covering the air because show memory was unreadable would be a bad
// trade.
func (dr *Director) buildBrief(ctx context.Context, st station.Station, kind string,
	just *live.Entry, up *live.Upcoming, maxChars int,
	daypartFrom string, silentForMin int) Brief {

	now := dr.d.Clock.Now().In(dr.d.Location)
	b := Brief{
		Type: kind, LocalTime: now.Format("Monday 15:04"),
		Daypart:  daypart(now.Hour()),
		MaxChars: maxChars,
	}
	if just != nil {
		b.JustPlayed = &BriefTrack{Title: just.Title, Artist: just.Artist, Source: just.Source,
			RequestedByName: just.RequestedByName, Reason: just.Reason}
	}
	switch kind {
	case live.ClipDaypartTransition:
		b.DaypartFrom = daypartFrom
	case live.ClipWakeGreeting:
		b.SilentForMin = silentForMin
	}

	var sessionStart time.Time
	if st.OnAirSince != nil {
		sessionStart = *st.OnAirSince
		b.OnAirForMin = int(dr.d.Clock.Now().Sub(sessionStart).Minutes())
	}
	if n, err := dr.d.Listeners.Count(ctx); err == nil {
		b.Listeners = n
	} else {
		dr.d.Logger.WarnContext(ctx, "director: listener count read failed", "err", err)
	}
	if up != nil {
		b.ComingUp = &BriefTrack{
			Title: up.Track.Title, Artist: up.Track.Channel, Source: up.Source,
			RequestedByName: up.RequestedByName, Reason: up.Reason,
		}
	}

	// AirHistory returns FINISHED tracks only, newest first, so the still-airing
	// anchor excludes itself and no extra filter is needed for it.
	if hist, err := dr.d.AirLog.History(ctx, tonightCap); err == nil {
		for i := len(hist) - 1; i >= 0; i-- { // reverse to oldest-first
			e := hist[i]
			if !sessionStart.IsZero() && e.StartedAt.Before(sessionStart) {
				continue // a previous broadcast is not "tonight"
			}
			b.Tonight = append(b.Tonight, BriefTrack{Title: e.Title, Artist: e.Artist})
		}
	} else {
		dr.d.Logger.WarnContext(ctx, "director: air history read failed", "err", err)
	}

	if dr.d.TalkMem != nil {
		if mem, err := dr.d.TalkMem.Recent(ctx, sessionStart, threadCap); err == nil {
			for _, m := range mem {
				b.Thread = append(b.Thread, m.Summary)
				b.RecentPhrases = append(b.RecentPhrases, m.Phrases...)
			}
		} else {
			dr.d.Logger.WarnContext(ctx, "director: show memory read failed", "err", err)
		}
	}
	return b
}

// briefFacts reads the two mu-guarded values the brief needs that are not on
// the station row. prepare runs off the audio hot path, so one uncontended
// lock acquisition here is cheaper than threading them through RunOnce.
func (dr *Director) briefFacts() (daypartFrom string, silentForMin int) {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	return dr.daypartFrom, dr.silentForMin
}

// newCorrelationID returns a random hex id grouping one prepare's LLM calls.
// crypto/rand rather than google/uuid: the module carries uuid only as an
// indirect dependency and no Go code in this repo generates one today
// (request ids come from Postgres), so this avoids promoting it.
func newCorrelationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "" // no id is better than a predictable one; the join just misses
	}
	return hex.EncodeToString(b[:])
}
