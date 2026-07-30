package director

// Brief is the data block for one seam break. It enters the prompt ONLY inside
// brain.BuildScriptPrompts' <brief> delimiter (data, not instructions);
// segment instructions ride exclusively in brain.SeamRules.
//
// Everything here except ComingUp is either derived from the station's own
// state or already aired, so it is TRUE and she may state it plainly. ComingUp
// is a promise the director has PINNED (spec §3) — also true. Song and artist
// colour is not in here at all: it comes from model knowledge and the rules
// require her to hedge it.
type Brief struct {
	Type        string `json:"type"` // "seam"
	LocalTime   string `json:"local_time"`
	Daypart     string `json:"daypart"`
	OnAirForMin int    `json:"on_air_for_min"`
	// Listeners is omitempty on purpose. RunOnce only reaches prepare when the
	// listener count read SUCCEEDED and was greater than zero, so a zero here
	// can only mean the second read failed — and shipping "listeners": 0 into a
	// brief whose contract is "these facts are true, state them plainly" would
	// have her announce an empty room that isn't empty.
	Listeners  int        `json:"listeners,omitempty"`
	JustPlayed BriefTrack `json:"just_played"`
	// ComingUp is nil when nothing could be promised — PeekNext found
	// planNext's unknowable lazy-shuffle arm. She then simply backsells,
	// which is exactly the pre-seam behaviour.
	ComingUp *BriefTrack `json:"coming_up,omitempty"`
	// Tonight is what has already aired THIS session, oldest first, so she
	// can thread the night together. Session-scoped on purpose.
	Tonight []BriefTrack `json:"tonight,omitempty"`
	// Thread is her own self-summaries this session, oldest first — continuity
	// she may build on and call back to.
	Thread []string `json:"thread,omitempty"`
	// RecentPhrases stays a don't-repeat blocklist, NOT material.
	RecentPhrases []string `json:"recent_phrases,omitempty"`
	MaxChars      int      `json:"max_chars"`
}

// BriefTrack is one track in the brief. Source/Reason/RequestedByName carry
// the v1.1 provenance vocabulary; RequestedByName is the ONLY
// listener-originated text allowed here (never free text — the call-in digest
// invariant is untouched).
type BriefTrack struct {
	Title           string `json:"title"`
	Artist          string `json:"artist,omitempty"`
	Source          string `json:"source,omitempty"` // "" shuffle | "listener" | "ai"
	RequestedByName string `json:"requested_by_name,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

// daypart names the Vietnamese broadcast daypart for a station-local hour.
func daypart(hour int) string {
	switch {
	case hour < 5:
		return "đêm khuya"
	case hour < 11:
		return "sáng"
	case hour < 14:
		return "trưa"
	case hour < 18:
		return "chiều"
	case hour < 22:
		return "tối"
	default:
		return "đêm"
	}
}

// talkRules is appended to the persona bible in the system prompt for
// backsell generation — the segment-specific rules the persona file doesn't
// carry. Instructions live here, never inside the brief.
const talkRules = `

## Luật talk break
- Đây là một talk break NGẮN giữa hai bài hát: nói về bài VỪA PHÁT xong (just_played).
- KHÔNG hứa hẹn bài "tiếp theo". Nếu nhắc queue_teasers, chỉ nói "sắp tới" chung chung.
- Nếu just_played có requested_by_name: cảm ơn người đó đã yêu cầu bài. Nếu có reason: có thể nhắc lại lý do chọn bài một cách tự nhiên.
- Đừng lặp lại ý trong memory_summaries hay câu trong recent_phrases.
- Mọi con số viết bằng chữ. Script ngắn hơn max_chars ký tự.`
