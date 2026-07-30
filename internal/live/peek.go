package live

import (
	"context"

	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/request"
	"github.com/the-algovn/radio-service/internal/schedule"
)

// Upcoming is what PeekNext believes airs next. Committed reports that it is
// ALREADY the committed next-up, so it is binding with no further write —
// the director skips its pin in that case.
type Upcoming struct {
	Track           library.Track
	RequestID       string // "" = a shuffle next-up
	Source          string
	RequestedByName string
	Reason          string
	Committed       bool
}

// PeekNext proposes what airs next, mirroring planNext's priority using reads
// only: the committed next-up, then the head of the ready queue.
//
// found=false means UNKNOWABLE, not "nothing airs": it is planNext's lazy
// shuffle arm, which rolls a fresh random pick at the boundary and therefore
// cannot be predicted. The director promises nothing in that case.
//
// This deliberately does NOT read the on-air flag or the library bed —
// planNext's stop/emptyLib arms are the feeder's business, and the director
// has its own wake gates. What it MUST match is the CHOICE, which
// TestPeekNextMatchesPlanNext pins.
//
// A peek is only ever a proposal. What makes it true is the pin: the director
// writes the track into schedule.NextUp, which planNext reads ahead of the
// request queue. Do not be tempted to trust a peek without pinning — the
// ready-queue head is not stable (see request.Store.NextReady).
func PeekNext(ctx context.Context, sched schedule.Store, reqs request.Store,
	lib library.Library) (Upcoming, bool, error) {

	nu, ok, err := sched.GetNextUp(ctx)
	if err != nil {
		return Upcoming{}, false, err
	}
	if ok {
		track, exists, gerr := lib.Get(ctx, nu.YTID)
		if gerr != nil {
			return Upcoming{}, false, gerr
		}
		if !exists {
			return Upcoming{}, false, nil // unusable commitment; promise nothing
		}
		up := Upcoming{Track: track, RequestID: nu.RequestID, Committed: true}
		if nu.RequestID != "" {
			req, found, rerr := reqs.Get(ctx, nu.RequestID)
			if rerr != nil {
				return Upcoming{}, false, rerr
			}
			if !found || req.Status != request.StatusReady {
				return Upcoming{}, false, nil
			}
			up.Source, up.RequestedByName, up.Reason = req.Source, req.DisplayName, req.Reason
		}
		return up, true, nil
	}

	req, found, err := reqs.NextReady(ctx)
	if err != nil {
		return Upcoming{}, false, err
	}
	if !found {
		return Upcoming{}, false, nil
	}
	track, exists, err := lib.Get(ctx, req.YTID)
	if err != nil {
		return Upcoming{}, false, err
	}
	if !exists {
		return Upcoming{}, false, nil
	}
	return Upcoming{Track: track, RequestID: req.ID, Source: req.Source,
		RequestedByName: req.DisplayName, Reason: req.Reason}, true, nil
}
