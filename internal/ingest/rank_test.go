package ingest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func c(id, title, channel string, dur, views int64) Candidate {
	return Candidate{YTID: id, Title: title, Channel: channel, DurationS: dur, ViewCount: views}
}

func TestRankPrefersTopicAndOfficial(t *testing.T) {
	got := Rank("em của ngày hôm qua", []Candidate{
		c("a", "Em Của Ngày Hôm Qua (Live at show)", "Some Fan", 260, 50_000),
		c("b", "Em Của Ngày Hôm Qua", "Sơn Tùng M-TP - Topic", 255, 9_000_000),
		c("c", "Em Của Ngày Hôm Qua | OFFICIAL MV", "Sơn Tùng M-TP Official", 300, 120_000_000),
	})
	require.Equal(t, "b", got[0].YTID) // Topic channel wins
	require.Equal(t, "c", got[1].YTID)
	require.Equal(t, "a", got[2].YTID) // live penalized
}

func TestRankQueryTokenExemptsPenalty(t *testing.T) {
	got := Rank("em của ngày hôm qua remix", []Candidate{
		c("x", "Em Của Ngày Hôm Qua Remix", "DJ Nào Đó", 250, 1_000_000),
		c("y", "Em Của Ngày Hôm Qua", "Sơn Tùng M-TP - Topic", 255, 9_000_000),
	})
	// "remix" was asked for → no penalty on x; topic bonus still applies to y.
	require.Equal(t, "y", got[0].YTID)
	for _, n := range got[1].Notes {
		require.NotContains(t, n, "penalty:remix")
	}
}

func TestRankDurationAndViews(t *testing.T) {
	got := Rank("bài gì đó", []Candidate{
		c("long", "Bài Gì Đó (Mixtape 1 hour)", "Chill Mix", 3600, 2_000_000),
		c("tiny", "Bài Gì Đó", "Ai Đó", 30, 500),
		c("ok", "Bài Gì Đó", "Ca Sĩ - Topic", 240, 800_000),
	})
	require.Equal(t, "ok", got[0].YTID)
}

func TestRankDedupeOverlappingPenalties(t *testing.T) {
	got := Rank("em của ngày hôm qua", []Candidate{
		c("remix", "Em Của Ngày Hôm Qua Remix", "DJ Nào Đó", 250, 1_000_000),
	})
	// "remix" contains "mix", but only "remix" should be penalized once (−25),
	// not both "remix" and "mix" (−50 total).
	scored := got[0]
	require.Equal(t, "remix", scored.YTID)
	penaltyCount := 0
	for _, note := range scored.Notes {
		if strings.HasPrefix(note, "penalty:") {
			penaltyCount++
		}
	}
	require.Equal(t, 1, penaltyCount, "expected exactly 1 penalty note, got %v", scored.Notes)
}

// "live" must not match inside Deliver/Olive, and "mix" must not match inside
// mixtape via a bare substring test.
func TestRankMatchesOnWordBoundaries(t *testing.T) {
	got := Rank("deliver me", []Candidate{
		c("d", "Deliver Me", "Ca Sĩ - Topic", 240, 500_000),
	})
	for _, n := range got[0].Notes {
		require.NotContains(t, n, "penalty:live",
			"'live' must not match inside 'Deliver': %v", got[0].Notes)
	}
}

// An Official Trailer currently scores +20 from the "official" bonus and beats
// a real song. Trailers are not music.
func TestRankDoesNotRewardTrailersAsOfficial(t *testing.T) {
	got := Rank("bài gì đó", []Candidate{
		c("trailer", "Bài Gì Đó | Official Trailer", "Studio", 150, 900_000),
		c("song", "Bài Gì Đó", "Ca Sĩ", 240, 400_000),
	})
	require.Equal(t, "song", got[0].YTID, "a real song must outrank an official trailer")
}

// The penalty list presupposed every result was already a song. Nothing caught
// a podcast, an interview, or a review.
func TestRankPenalizesNonMusicFormats(t *testing.T) {
	for _, title := range []string{
		"Podcast tập 42 — chuyện đêm khuya",
		"Phỏng vấn độc quyền với ca sĩ",
		"Reaction: nghe lần đầu",
		"Review phim mới nhất",
	} {
		got := Rank("chuyện đêm khuya", []Candidate{
			c("x", title, "Kênh Nào Đó", 240, 500_000),
			c("song", "Chuyện Đêm Khuya", "Ca Sĩ - Topic", 240, 500_000),
		})
		require.Equal(t, "song", got[0].YTID, "a song must outrank %q", title)
	}
}

// Vietnamese compilation vocabulary is what a mood query actually returns.
func TestRankPenalizesVietnameseCompilations(t *testing.T) {
	got := Rank("nhạc trữ tình", []Candidate{
		c("comp", "Tuyển Tập Nhạc Trữ Tình Hay Nhất", "Kênh Nhạc", 400, 5_000_000),
		c("song", "Nhạc Trữ Tình", "Ca Sĩ - Topic", 240, 500_000),
	})
	require.Equal(t, "song", got[0].YTID)
}
