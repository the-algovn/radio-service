package director

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBriefJSONFieldNames(t *testing.T) {
	b, err := json.Marshal(Brief{
		Type: "backsell", LocalTime: "Thứ Ba 23:15", Daypart: "đêm",
		JustPlayed:      BriefTrack{Title: "Bài A", Artist: "Ca sĩ B", Source: "ai", Reason: "hợp đêm mưa"},
		QueueTeasers:    []string{"Bài C"},
		MemorySummaries: []string{"đã kể chuyện mưa"},
		RecentPhrases:   []string{"bạn nghe đài"},
		MaxChars:        450,
	})
	require.NoError(t, err)
	s := string(b)
	for _, key := range []string{`"type"`, `"local_time"`, `"daypart"`, `"just_played"`,
		`"title"`, `"artist"`, `"source"`, `"reason"`,
		`"queue_teasers"`, `"memory_summaries"`, `"recent_phrases"`, `"max_chars"`} {
		require.Contains(t, s, key)
	}
	require.NotContains(t, s, `"requested_by_name"`, "empty omitempty field must be absent")
}

func TestDaypartMapping(t *testing.T) {
	cases := map[int]string{0: "đêm khuya", 4: "đêm khuya", 5: "sáng", 10: "sáng",
		11: "trưa", 13: "trưa", 14: "chiều", 17: "chiều", 18: "tối", 21: "tối", 22: "đêm", 23: "đêm"}
	for h, want := range cases {
		require.Equal(t, want, daypart(h), "hour %d", h)
	}
}

func TestTalkRulesForbidPromisingNext(t *testing.T) {
	require.Contains(t, talkRules, "sắp tới")
	require.Contains(t, talkRules, "KHÔNG hứa")
}
