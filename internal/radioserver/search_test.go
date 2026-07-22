package radioserver

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	radiov1 "github.com/the-algovn/protos/gen/go/algovn/radio/v1"
	"github.com/the-algovn/radio-service/internal/ingest"
)

type fakeSearch struct {
	cs    []ingest.Candidate
	calls int
}

func (f *fakeSearch) Search(context.Context, string, int) ([]ingest.Candidate, error) {
	f.calls++
	return f.cs, nil
}

func TestSearchCandidatesAuthAndValidation(t *testing.T) {
	s := newTestServer(t)
	_, err := s.SearchCandidates(context.Background(), &radiov1.SearchCandidatesRequest{Query: "x"})
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	ctx := authCtx(t, map[string]string{"sub": "u1", "name": "Ngọc"})
	_, err = s.SearchCandidates(ctx, &radiov1.SearchCandidatesRequest{Query: "   "})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = s.SearchCandidates(ctx, &radiov1.SearchCandidatesRequest{Query: strings.Repeat("q", 201)})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestSearchCandidatesRanksAndCaps(t *testing.T) {
	s := newTestServer(t)
	search := &fakeSearch{}
	for i := 0; i < 9; i++ { // 9 plain candidates + 1 topic-channel one
		search.cs = append(search.cs, ingest.Candidate{
			YTID: fmt.Sprintf("v%d", i), Title: fmt.Sprintf("Bài %d", i),
			Channel: "kênh thường", DurationS: 240, ViewCount: 50_000,
		})
	}
	search.cs = append(search.cs, ingest.Candidate{
		YTID: "topic", Title: "Bài Chuẩn", Channel: "Ca Sĩ - Topic",
		DurationS: 250, ViewCount: 90_000, ThumbnailURL: "https://img/t",
	})
	s.deps.Search = search

	ctx := authCtx(t, map[string]string{"sub": "u1"})
	resp, err := s.SearchCandidates(ctx, &radiov1.SearchCandidatesRequest{Query: "bài"})
	require.NoError(t, err)
	require.Len(t, resp.GetCandidates(), 8) // top 8 of 10
	top := resp.GetCandidates()[0]
	require.Equal(t, "topic", top.GetYtId()) // rank put the Topic channel first
	require.Equal(t, int32(250), top.GetDurationS())
	require.Equal(t, "https://img/t", top.GetThumbnailUrl())
}

func TestSearchCandidatesRateLimited(t *testing.T) {
	s := newTestServer(t)
	s.deps.Search = &fakeSearch{}
	ctx := authCtx(t, map[string]string{"sub": "u1"})
	for i := 0; i < 10; i++ {
		_, err := s.SearchCandidates(ctx, &radiov1.SearchCandidatesRequest{Query: "q"})
		require.NoError(t, err, "call %d", i)
	}
	_, err := s.SearchCandidates(ctx, &radiov1.SearchCandidatesRequest{Query: "q"})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "tìm nhanh quá, chờ chút nha")
}
