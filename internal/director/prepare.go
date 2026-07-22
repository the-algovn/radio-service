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

	"github.com/the-algovn/radio-service/internal/brain"
	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/persona"
	"github.com/the-algovn/radio-service/internal/spend"
	"github.com/the-algovn/radio-service/internal/voice"
)

// prepare runs one whole clip-preparation attempt: script → TTS → render.
// Every failure is a quiet skip (logged, temp files removed, spend already
// ledgered stays ledgered) — the air never waits on this path.
func (dr *Director) prepare(ctx context.Context, kind string) (live.Clip, bool) {
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
	default: // live.ClipBacksell
		entry, found, err := dr.d.AirLog.Latest(ctx)
		if err != nil || !found {
			if err != nil {
				dr.d.Logger.Error("director: air log read failed", "err", err)
			}
			return live.Clip{}, false // nothing airing → nothing to talk about
		}
		anchorYTID, anchorStartedAt = entry.YTID, entry.StartedAt
		pers, err := persona.Load(dr.d.PersonaDir)
		if err != nil {
			dr.d.Logger.Error("director: persona load failed", "err", err)
			return live.Clip{}, false
		}
		briefJSON, err := json.Marshal(dr.buildBrief(ctx, entry))
		if err != nil {
			dr.d.Logger.Error("director: brief marshal failed", "err", err)
			return live.Clip{}, false
		}
		system, user := brain.BuildPrompts(pers+talkRules, string(briefJSON))
		var ok bool
		out, ok = dr.generateValid(ctx, system, user)
		if !ok {
			return live.Clip{}, false
		}
		script = out.Script
	}

	data, ext, err := dr.d.Voice.Synthesize(ctx, script, dr.d.VoiceID, dr.d.Rate)
	if err != nil {
		dr.d.Logger.Error("director: tts failed", "kind", kind, "err", err)
		return live.Clip{}, false
	}
	chars := utf8.RuneCountInString(script)
	cost := voice.CostUSD(dr.d.VoiceID, chars)
	provider := "google"
	if dr.d.VoiceFake {
		cost, provider = 0, "fake"
	}
	if lerr := dr.d.Ledger.Append(ctx, spend.Line{
		TS: time.Now(), Kind: "tts", Provider: provider, Label: "director:" + kind,
		Chars: chars, CostUSD: cost,
	}); lerr != nil {
		dr.d.Logger.Error("director: ledger append failed", "err", lerr)
	}

	n := dr.seq.Add(1)
	takePath := filepath.Join(dr.d.DataDir, fmt.Sprintf("take-%d.%s", n, ext))
	if err := os.WriteFile(takePath, data, 0o644); err != nil {
		dr.d.Logger.Error("director: take write failed", "err", err)
		return live.Clip{}, false
	}
	defer func() { _ = os.Remove(takePath) }()

	outPath := filepath.Join(dr.d.DataDir, fmt.Sprintf("clip-%d.pcm", n))
	durS, err := dr.d.Render(ctx, takePath, outPath)
	if err != nil {
		dr.d.Logger.Error("director: render failed", "kind", kind, "err", err)
		_ = os.Remove(outPath)
		return live.Clip{}, false
	}

	if kind == live.ClipBacksell {
		dr.pushRing(out.Summary, out.UsedPhrases)
	}
	dr.d.Logger.Info("talk clip prepared", "kind", kind, "duration_s", durS, "script", script)
	return live.Clip{Path: outPath, DurationS: durS, Script: script, Kind: kind,
		AnchorYTID: anchorYTID, AnchorStartedAt: anchorStartedAt}, true
}

// generateValid makes the script call with the on-air validation loop: parse
// failure aborts; validation violations get ONE retry with the violations
// appended; every attempt is priced before parsing ("tokens were spent
// either way").
func (dr *Director) generateValid(ctx context.Context, system, user string) (brain.Output, bool) {
	for attempt := 0; ; attempt++ {
		raw, usage, err := dr.d.Model.Generate(ctx, system, user)
		if err != nil {
			dr.d.Logger.Error("director: model call failed", "err", err)
			return brain.Output{}, false
		}
		cost := brain.CostUSD(dr.d.Model.Name(), usage)
		if lerr := dr.d.Ledger.Append(ctx, spend.Line{
			TS: time.Now(), Kind: "llm", Provider: dr.d.Model.Name(), Label: "director:backsell",
			InTokens: usage.In, OutTokens: usage.Out, CostUSD: cost,
		}); lerr != nil {
			dr.d.Logger.Error("director: ledger append failed", "err", lerr)
		}
		out, perr := brain.ParseOutput(raw)
		if perr != nil {
			dr.d.Logger.Error("director: parse failed", "err", perr, "raw", raw[:min(len(raw), 200)])
			return brain.Output{}, false
		}
		v := brain.Validate(out.Script, dr.d.MaxChars)
		if len(v) == 0 {
			return out, true
		}
		if attempt > 0 {
			dr.d.Logger.Warn("director: script invalid after retry; giving up", "violations", v)
			return brain.Output{}, false
		}
		user = user + "\n\nLỗi cần sửa (viết lại toàn bộ):\n- " + strings.Join(v, "\n- ")
	}
}

// buildBrief assembles the backsell data block from the currently-airing
// entry (the freshness anchor), queue teasers, and the show-memory ring.
func (dr *Director) buildBrief(ctx context.Context, just live.Entry) Brief {
	now := dr.d.Clock.Now().In(dr.d.Location)
	b := Brief{
		Type: "backsell", LocalTime: now.Format("Monday 15:04"), Daypart: daypart(now.Hour()),
		JustPlayed: BriefTrack{Title: just.Title, Artist: just.Artist, Source: just.Source,
			RequestedByName: just.RequestedByName, Reason: just.Reason},
		MaxChars: dr.d.MaxChars,
	}
	if pending, err := dr.d.Requests.Pending(ctx); err == nil {
		for _, it := range pending {
			if len(b.QueueTeasers) >= teaserCap {
				break
			}
			b.QueueTeasers = append(b.QueueTeasers, it.Title)
		}
	}
	dr.mu.Lock()
	for _, m := range dr.ring {
		b.MemorySummaries = append(b.MemorySummaries, m.summary)
		b.RecentPhrases = append(b.RecentPhrases, m.phrases...)
	}
	dr.mu.Unlock()
	return b
}
