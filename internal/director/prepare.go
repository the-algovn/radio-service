package director

import (
	"context"
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
	"github.com/the-algovn/radio-service/internal/spend"
	"github.com/the-algovn/radio-service/internal/station"
	"github.com/the-algovn/radio-service/internal/voice"
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
	ctx, cancel := context.WithTimeout(ctx, prepDeadline)
	defer cancel()

	var script, anchorYTID string
	var anchorStartedAt time.Time
	var out brain.Output

	switch kind {
	case live.ClipStationID:
		line, ok := dr.ids.next()
		if !ok {
			return live.Clip{}, false
		}
		script = line
	default: // live.ClipSeam
		entry, found, err := dr.d.AirLog.Latest(ctx)
		if err != nil || !found {
			if err != nil {
				dr.d.Logger.ErrorContext(ctx, "director: air log read failed", "err", err)
			}
			return live.Clip{}, false // nothing airing → nothing to talk about
		}
		anchorYTID, anchorStartedAt = entry.YTID, entry.StartedAt
		pers, err := persona.Load(dr.d.PersonaDir)
		if err != nil {
			dr.d.Logger.ErrorContext(ctx, "director: persona load failed", "err", err)
			return live.Clip{}, false
		}
		briefJSON, err := json.Marshal(dr.buildBrief(ctx, st, entry, nil, dj.MaxChars))
		if err != nil {
			dr.d.Logger.ErrorContext(ctx, "director: brief marshal failed", "err", err)
			return live.Clip{}, false
		}
		system, user := brain.BuildPrompts(pers+talkRules, string(briefJSON))
		var ok bool
		out, ok = dr.generateValid(ctx, system, user, dj.MaxChars)
		if !ok {
			return live.Clip{}, false
		}
		script = out.Script
	}

	data, ext, err := dr.d.Voice.Synthesize(ctx, script, dj.VoiceID, dj.Rate)
	if err != nil {
		dr.d.Logger.ErrorContext(ctx, "director: tts failed", "kind", kind, "err", err)
		return live.Clip{}, false
	}
	chars := utf8.RuneCountInString(script)
	cost := voice.CostUSD(dj.VoiceID, chars)
	provider := "google"
	if dr.d.VoiceFake {
		cost, provider = 0, "fake"
	}
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

	if kind == live.ClipSeam {
		dr.pushRing(out.Summary, out.UsedPhrases)
	}
	dr.d.Logger.InfoContext(ctx, "talk clip prepared", "kind", kind, "duration_s", durS, "script", script)
	return live.Clip{Path: outPath, DurationS: durS, Script: script, Kind: kind,
		AnchorYTID: anchorYTID, AnchorStartedAt: anchorStartedAt}, true
}

// generateValid makes the script call with the on-air validation loop: parse
// failure aborts; validation violations get ONE retry with the violations
// appended. Cost and the spend ledger are handled by the Eino audit callback,
// so this loop no longer prices anything itself.
func (dr *Director) generateValid(ctx context.Context, system, user string, maxChars int) (brain.Output, bool) {
	ctx = audit.WithLabel(ctx, "director:seam")
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

// buildBrief assembles the seam-break data block. up is nil when nothing could
// be promised. Every read here is best-effort: a failure degrades one FIELD,
// never the break — music covering the air because show memory was unreadable
// would be a bad trade.
func (dr *Director) buildBrief(ctx context.Context, st station.Station,
	just live.Entry, up *live.Upcoming, maxChars int) Brief {

	now := dr.d.Clock.Now().In(dr.d.Location)
	b := Brief{
		Type: live.ClipSeam, LocalTime: now.Format("Monday 15:04"),
		Daypart: daypart(now.Hour()),
		JustPlayed: BriefTrack{Title: just.Title, Artist: just.Artist, Source: just.Source,
			RequestedByName: just.RequestedByName, Reason: just.Reason},
		MaxChars: maxChars,
	}

	var sessionStart time.Time
	if st.OnAirSince != nil {
		sessionStart = *st.OnAirSince
		b.OnAirForMin = int(dr.d.Clock.Now().Sub(sessionStart).Minutes())
	}
	if n, err := dr.d.Listeners.Count(ctx); err == nil {
		b.Listeners = n
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
