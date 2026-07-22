package radioserver

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	radiov1 "github.com/the-algovn/protos/gen/go/algovn/radio/v1"
	"github.com/the-algovn/radio-service/internal/request"
)

type fakeSkipper struct{ calls int }

func (f *fakeSkipper) RequestSkip() { f.calls++ }

func seedPending(t *testing.T, s *Server) (listenerID, aiID string) {
	t.Helper()
	ctx := context.Background()
	l, err := s.deps.Requests.Create(ctx, request.Item{Source: request.SourceListener,
		RequestedBy: "u1", DisplayName: "Ngọc", YTID: "l1", Title: "L1", Channel: "c",
		DurationS: 100, Status: request.StatusReady})
	require.NoError(t, err)
	a, err := s.deps.Requests.Create(ctx, request.Item{Source: request.SourceAI,
		YTID: "a1", Title: "A1", Channel: "c", DurationS: 100,
		Status: request.StatusApproved, Reason: "đổi gió"})
	require.NoError(t, err)
	return l.ID, a.ID
}

func TestListStationRequests(t *testing.T) {
	s := newTestServer(t)
	lID, aID := seedPending(t, s)
	ctx := context.Background()
	// one terminal row for the recent list
	term, err := s.deps.Requests.Create(ctx, request.Item{Source: request.SourceListener,
		RequestedBy: "u2", YTID: "t1", Title: "T1", Channel: "c", DurationS: 100,
		Status: request.StatusReady})
	require.NoError(t, err)
	require.NoError(t, s.deps.Requests.MarkAired(ctx, term.ID, time.Now()))

	resp, err := s.ListStationRequests(ctx, &radiov1.ListStationRequestsRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetPending(), 2)
	require.Equal(t, lID, resp.GetPending()[0].GetId()) // listener first (natural tier)
	require.Equal(t, aID, resp.GetPending()[1].GetId())
	require.Equal(t, "đổi gió", resp.GetPending()[1].GetReason()) // TrackRequest.reason populated
	require.Len(t, resp.GetRecent(), 1)
	require.Equal(t, "aired", resp.GetRecent()[0].GetStatus())
}

func TestReorderRequests(t *testing.T) {
	s := newTestServer(t)
	lID, aID := seedPending(t, s)
	ctx := context.Background()

	resp, err := s.ReorderRequests(ctx, &radiov1.ReorderRequestsRequest{Ids: []string{aID, lID}})
	require.NoError(t, err)
	require.Equal(t, aID, resp.GetPending()[0].GetId()) // pinned order rules

	_, err = s.ReorderRequests(ctx, &radiov1.ReorderRequestsRequest{Ids: []string{aID}})
	require.Equal(t, codes.InvalidArgument, status.Code(err)) // stale set
	_, err = s.ReorderRequests(ctx, &radiov1.ReorderRequestsRequest{})
	require.Equal(t, codes.InvalidArgument, status.Code(err)) // empty
}

func TestRemoveRequest(t *testing.T) {
	s := newTestServer(t)
	lID, _ := seedPending(t, s)
	ctx := context.Background()

	resp, err := s.RemoveRequest(ctx, &radiov1.RemoveRequestRequest{Id: lID})
	require.NoError(t, err)
	require.Len(t, resp.GetPending(), 1) // only the AI row remains
	mine, err := s.deps.Requests.ByUser(ctx, "u1", 1)
	require.NoError(t, err)
	require.Equal(t, request.StatusFailed, mine[0].Status)
	require.Equal(t, "đài đã gỡ yêu cầu này", mine[0].FailReason)

	_, err = s.RemoveRequest(ctx, &radiov1.RemoveRequestRequest{Id: lID}) // already terminal
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	_, err = s.RemoveRequest(ctx, &radiov1.RemoveRequestRequest{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestSkipTrack(t *testing.T) {
	s := newTestServer(t, "a") // non-empty library so GoOnAir works
	ctx := context.Background()

	_, err := s.SkipTrack(ctx, &radiov1.SkipTrackRequest{})
	require.Equal(t, codes.FailedPrecondition, status.Code(err)) // off air

	_, err = s.GoOnAir(ctx, &radiov1.GoOnAirRequest{})
	require.NoError(t, err)
	_, err = s.SkipTrack(ctx, &radiov1.SkipTrackRequest{})
	require.NoError(t, err)
	require.Equal(t, 1, s.deps.Skipper.(*fakeSkipper).calls)
}

func TestSetAIEnabledAndStationStats(t *testing.T) {
	s := newTestServer(t, "a", "b")
	ctx := context.Background()

	off, err := s.SetAIEnabled(ctx, &radiov1.SetAIEnabledRequest{Enabled: false})
	require.NoError(t, err)
	require.False(t, off.GetStation().GetAiEnabled())

	st, err := s.GetStation(ctx, &radiov1.GetStationRequest{})
	require.NoError(t, err)
	require.False(t, st.GetStation().GetAiEnabled())
	require.Equal(t, int32(2), st.GetStats().GetLibraryCount())
	require.Equal(t, 1.0, st.GetStats().GetBudgetUsd())
	require.Equal(t, 0.25, st.GetStats().GetSpendTodayUsd())
}
