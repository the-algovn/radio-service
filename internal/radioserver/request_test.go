package radioserver

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	radiov1 "github.com/the-algovn/protos/gen/go/algovn/radio/v1"
	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/live"
	"github.com/the-algovn/radio-service/internal/request"
)

func cand(ytID string, durS int32) *radiov1.Candidate {
	return &radiov1.Candidate{YtId: ytID, Title: "t-" + ytID, Channel: "c-" + ytID,
		DurationS: durS, ThumbnailUrl: "https://img/" + ytID}
}

func TestRequestTrackHappyPathUncachedAndCached(t *testing.T) {
	s := newTestServer(t, "cached1")
	ctx := authCtx(t, map[string]string{"sub": "u1", "name": "Ngọc"})

	resp, err := s.RequestTrack(ctx, &radiov1.RequestTrackRequest{Candidate: cand("new1", 240)})
	require.NoError(t, err)
	r := resp.GetRequest()
	require.Equal(t, "listener", r.GetSource())
	require.Equal(t, "Ngọc", r.GetRequestedByName())
	require.Equal(t, "approved", r.GetStatus()) // not in library → awaits ingest
	require.Equal(t, int32(240), r.GetDurationS())
	require.NotEmpty(t, r.GetId())
	require.NotEmpty(t, r.GetCreatedAt())

	resp, err = s.RequestTrack(ctx, &radiov1.RequestTrackRequest{Candidate: cand("cached1", 60)})
	require.NoError(t, err)
	require.Equal(t, "ready", resp.GetRequest().GetStatus()) // library hit → instantly airable
}

func TestRequestTrackValidation(t *testing.T) {
	s := newTestServer(t)
	_, err := s.RequestTrack(context.Background(), &radiov1.RequestTrackRequest{Candidate: cand("x", 100)})
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	ctx := authCtx(t, map[string]string{"sub": "u1"})
	_, err = s.RequestTrack(ctx, &radiov1.RequestTrackRequest{})
	require.Equal(t, codes.InvalidArgument, status.Code(err)) // no candidate
	_, err = s.RequestTrack(ctx, &radiov1.RequestTrackRequest{Candidate: &radiov1.Candidate{Title: "no id", DurationS: 100}})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = s.RequestTrack(ctx, &radiov1.RequestTrackRequest{Candidate: &radiov1.Candidate{YtId: "x", DurationS: 100}})
	require.Equal(t, codes.InvalidArgument, status.Code(err)) // no title
	_, err = s.RequestTrack(ctx, &radiov1.RequestTrackRequest{Candidate: cand("x", 0)})
	require.Equal(t, codes.InvalidArgument, status.Code(err)) // no duration
	_, err = s.RequestTrack(ctx, &radiov1.RequestTrackRequest{Candidate: cand("x", 601)})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "bài dài quá mười phút, đài không phát được")
}

// The library row is ground truth for a cached track: a client claiming
// 240s for a yt_id whose already-downloaded/probed duration is 700s must be
// rejected, even though the client-supplied duration alone would pass.
func TestRequestTrackCachedLibraryDurationIsGroundTruth(t *testing.T) {
	s := newTestServer(t)
	require.NoError(t, s.deps.Library.Add(context.Background(), library.Track{
		YTID: "long1", Title: "t-long1", Channel: "c-long1", DurationS: 700, ArtifactID: "a-long1",
	}))
	ctx := authCtx(t, map[string]string{"sub": "u1"})

	_, err := s.RequestTrack(ctx, &radiov1.RequestTrackRequest{Candidate: cand("long1", 240)})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), msgTooLong)
}

func TestRequestTrackTitleAndChannelLengthCap(t *testing.T) {
	s := newTestServer(t)
	ctx := authCtx(t, map[string]string{"sub": "u1"})

	_, err := s.RequestTrack(ctx, &radiov1.RequestTrackRequest{Candidate: &radiov1.Candidate{
		YtId: "t1", Title: strings.Repeat("x", 201), Channel: "c", DurationS: 100,
	}})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = s.RequestTrack(ctx, &radiov1.RequestTrackRequest{Candidate: &radiov1.Candidate{
		YtId: "t2", Title: "t", Channel: strings.Repeat("y", 201), DurationS: 100,
	}})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestRequestTrackGuards(t *testing.T) {
	s := newTestServer(t)
	u1 := authCtx(t, map[string]string{"sub": "u1", "name": "A"})
	u2 := authCtx(t, map[string]string{"sub": "u2", "name": "B"})

	// pending cap: 3 per user
	for i, id := range []string{"p1", "p2", "p3"} {
		_, err := s.RequestTrack(u1, &radiov1.RequestTrackRequest{Candidate: cand(id, 100)})
		require.NoError(t, err, "request %d", i)
	}
	_, err := s.RequestTrack(u1, &radiov1.RequestTrackRequest{Candidate: cand("p4", 100)})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "bạn đang có ba bài chờ phát rồi, đợi chút nha")

	// duplicate: any pending yt_id, even another user's
	_, err = s.RequestTrack(u2, &radiov1.RequestTrackRequest{Candidate: cand("p1", 100)})
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "bài này đang trong hàng đợi rồi, sắp phát thôi")

	// recently aired (< 2h, via the AIR LOG — shuffle plays have no request row)
	require.NoError(t, s.deps.Log.Append(context.Background(), live.Entry{
		YTID: "aired1", Title: "t", Artist: "a",
		StartedAt: time.Now().Add(-30 * time.Minute), DurationS: 240,
	}))
	_, err = s.RequestTrack(u2, &radiov1.RequestTrackRequest{Candidate: cand("aired1", 100)})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "vừa phát xong, để khuya nhé")

	// aired > 2h ago is fine again
	require.NoError(t, s.deps.Log.Append(context.Background(), live.Entry{
		YTID: "aired2", Title: "t", Artist: "a",
		StartedAt: time.Now().Add(-3 * time.Hour), DurationS: 240,
	}))
	_, err = s.RequestTrack(u2, &radiov1.RequestTrackRequest{Candidate: cand("aired2", 100)})
	require.NoError(t, err)
}

func TestRequestTrackDailyQuota(t *testing.T) {
	s := newTestServer(t)
	ctx := authCtx(t, map[string]string{"sub": "u1"})
	// pre-seed 10 requests today, all already aired (so the pending cap
	// can't fire first) — the daily count still includes them.
	for i := 0; i < 10; i++ {
		it, err := s.deps.Requests.Create(context.Background(), request.Item{
			Source: request.SourceListener, RequestedBy: "u1", YTID: fmt.Sprintf("d%d", i),
			Title: "t", Channel: "c", DurationS: 100, Status: request.StatusReady,
		})
		require.NoError(t, err)
		require.NoError(t, s.deps.Requests.MarkAired(context.Background(), it.ID, time.Now()))
	}
	_, err := s.RequestTrack(ctx, &radiov1.RequestTrackRequest{Candidate: cand("one-more", 100)})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "hôm nay bạn yêu cầu đủ mười bài rồi, mai lại nhé")
}

func TestListMyRequests(t *testing.T) {
	s := newTestServer(t)
	u1 := authCtx(t, map[string]string{"sub": "u1", "name": "A"})
	_, err := s.ListMyRequests(context.Background(), &radiov1.ListMyRequestsRequest{})
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	_, err = s.RequestTrack(u1, &radiov1.RequestTrackRequest{Candidate: cand("m1", 100)})
	require.NoError(t, err)
	_, err = s.RequestTrack(authCtx(t, map[string]string{"sub": "u2"}), &radiov1.RequestTrackRequest{Candidate: cand("m2", 100)})
	require.NoError(t, err)

	resp, err := s.ListMyRequests(u1, &radiov1.ListMyRequestsRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetRequests(), 1)
	require.Equal(t, "m1", resp.GetRequests()[0].GetYtId())
}

func TestGetQueueReturnsPendingWithBadges(t *testing.T) {
	s := newTestServer(t)
	ctx0 := context.Background()
	_, err := s.deps.Requests.Create(ctx0, request.Item{Source: request.SourceAI,
		YTID: "ai1", Title: "AI Pick", Channel: "c", DurationS: 100, Status: request.StatusReady,
		Reason: "đổi gió"})
	require.NoError(t, err)
	_, err = s.deps.Requests.Create(ctx0, request.Item{Source: request.SourceListener,
		RequestedBy: "u1", DisplayName: "Ngọc", YTID: "l1", Title: "Yêu Cầu", Channel: "c",
		DurationS: 100, ThumbnailURL: "https://img/l1", Status: request.StatusApproved})
	require.NoError(t, err)

	resp, err := s.GetQueue(ctx0, &radiov1.GetQueueRequest{})
	require.NoError(t, err)
	items := resp.GetItems()
	require.Len(t, items, 2)
	// listener first even though the AI row is older
	require.Equal(t, "Yêu Cầu", items[0].GetTitle())
	require.Equal(t, "listener", items[0].GetSource())
	require.Equal(t, "Ngọc", items[0].GetRequestedByName())
	require.Equal(t, "https://img/l1", items[0].GetThumbnailUrl())
	require.Equal(t, "ai", items[1].GetSource())
	require.False(t, items[0].GetHasDedication())
	require.Equal(t, "đổi gió", items[1].GetReason())
	require.Empty(t, items[0].GetReason()) // listener rows carry no reason
}

func TestGoOnAirNeedsNonEmptyLibrary(t *testing.T) {
	s := newTestServer(t) // no tracks
	_, err := s.GoOnAir(context.Background(), &radiov1.GoOnAirRequest{})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	s2 := newTestServer(t, "a")
	on, err := s2.GoOnAir(context.Background(), &radiov1.GoOnAirRequest{})
	require.NoError(t, err)
	require.True(t, on.GetStation().GetOnAir())
}
