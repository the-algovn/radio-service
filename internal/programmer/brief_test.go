package programmer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParsePicks(t *testing.T) {
	picks, err := ParsePicks("```json\n{\"picks\":[{\"query\":\"nhạc Trịnh đêm khuya\",\"reason\":\"khuya rồi\"},{\"yt_id\":\"abc123\",\"reason\":\"đổi không khí\"}]}\n```")
	require.NoError(t, err)
	require.Len(t, picks, 2)
	require.Equal(t, "nhạc Trịnh đêm khuya", picks[0].Query)
	require.Empty(t, picks[0].YTID)
	require.Equal(t, "abc123", picks[1].YTID)

	// invalid picks are dropped; >2 truncates to 2
	picks, err = ParsePicks(`{"picks":[{"reason":"no target"},{"query":"a"},{"query":"b"},{"query":"c"}]}`)
	require.NoError(t, err)
	require.Len(t, picks, 2)
	require.Equal(t, "a", picks[0].Query)

	// both query and yt_id set → invalid pick
	_, err = ParsePicks(`{"picks":[{"query":"x","yt_id":"y"}]}`)
	require.Error(t, err)
	_, err = ParsePicks(`{"picks":[]}`)
	require.Error(t, err)
	_, err = ParsePicks("not json")
	require.Error(t, err)
}

func TestBuildPromptsDelimitsBrief(t *testing.T) {
	system, user := BuildPrompts("PERSONA BIBLE", `{"local_time":"23:15"}`)
	require.Contains(t, system, "PERSONA BIBLE")
	require.Contains(t, system, `"picks"`)
	require.Contains(t, user, "<brief>")
	require.Contains(t, user, `{"local_time":"23:15"}`)
	require.Contains(t, user, "</brief>")
}
