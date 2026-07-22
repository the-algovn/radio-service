package director

// Brief is the data block for one backsell segment. It enters the prompt
// ONLY inside brain.BuildPrompts' <brief> delimiter (data, not instructions);
// director-specific instructions ride exclusively in talkRules, appended to
// the persona in the system prompt.
type Brief struct {
	Type            string     `json:"type"` // "backsell"
	LocalTime       string     `json:"local_time"`
	Daypart         string     `json:"daypart"`
	JustPlayed      BriefTrack `json:"just_played"`
	QueueTeasers    []string   `json:"queue_teasers,omitempty"`
	MemorySummaries []string   `json:"memory_summaries,omitempty"`
	RecentPhrases   []string   `json:"recent_phrases,omitempty"`
	MaxChars        int        `json:"max_chars"`
}

// BriefTrack is the track the break talks about — the one airing at prep
// time, which will have just finished when the break airs. Source/Reason/
// RequestedByName carry the v1.1 provenance vocabulary; RequestedByName is
// the ONLY listener-originated text allowed here (never free text — the
// call-in digest invariant stays untouched in v2).
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
