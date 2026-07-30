package director

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBriefJSONFieldNames(t *testing.T) {
	b, err := json.Marshal(Brief{
		Type: "seam", LocalTime: "Thứ Ba 23:15", Daypart: "đêm",
		OnAirForMin:   120,
		Listeners:     5,
		JustPlayed:    BriefTrack{Title: "Bài A", Artist: "Ca sĩ B", Source: "ai", Reason: "hợp đêm mưa"},
		Tonight:       []BriefTrack{{Title: "Bài C"}},
		Thread:        []string{"đã kể chuyện mưa"},
		RecentPhrases: []string{"bạn nghe đài"},
		MaxChars:      450,
	})
	require.NoError(t, err)
	s := string(b)
	for _, key := range []string{`"type"`, `"local_time"`, `"daypart"`, `"on_air_for_min"`,
		`"listeners"`, `"just_played"`, `"title"`, `"artist"`, `"source"`, `"reason"`,
		`"tonight"`, `"thread"`, `"recent_phrases"`, `"max_chars"`} {
		require.Contains(t, s, key)
	}
	require.NotContains(t, s, `"requested_by_name"`, "empty omitempty field must be absent")
	require.NotContains(t, s, `"coming_up"`, "empty omitempty field must be absent")
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

func TestBriefMarshalsComingUpOnlyWhenPresent(t *testing.T) {
	b := Brief{Type: "seam", JustPlayed: BriefTrack{Title: "A"}, MaxChars: 1500}
	j, err := json.Marshal(b)
	require.NoError(t, err)
	require.NotContains(t, string(j), "coming_up",
		"an absent promise must not appear as a null the model could read")

	b.ComingUp = &BriefTrack{Title: "B", Artist: "Sơn Tùng M-TP"}
	j, err = json.Marshal(b)
	require.NoError(t, err)
	require.Contains(t, string(j), `"coming_up":{"title":"B","artist":"Sơn Tùng M-TP"}`)
}

func TestBriefTonightCarriesOnlyTitleAndArtist(t *testing.T) {
	b := Brief{Tonight: []BriefTrack{{Title: "A", Artist: "X"}}}
	j, err := json.Marshal(b)
	require.NoError(t, err)
	require.Contains(t, string(j), `"tonight":[{"title":"A","artist":"X"}]`)
}
