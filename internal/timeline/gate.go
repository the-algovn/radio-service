package timeline

// Gate reports why no talk break is coming, or GateOK.
//
// Order is deliberate and names the thing to fix FIRST, not the thing that is
// technically most upstream. An off-air station also has zero listeners and
// may also have the DJ disabled; telling the operator "no listeners" there is
// useless. After off-air the order sorts by cost of the fix: a redeploy, a
// console toggle, a budget, then an audience.
//
// It deliberately does NOT gate on a prepared clip. A clip already in the slot
// is paid for and Take will still air it — the gates are evaluated at PREPARE
// time, not at Take — so suppressing it would hide a break that is about to
// happen. See the walk: only `due` is suppressed.
func Gate(s State) string {
	switch {
	case !s.Station.OnAir:
		return GateOffAir
	case !s.Dir.Present:
		return GateDJDisabled
	case !s.Station.AIEnabled:
		return GateAIPaused
	case s.BudgetUSD > 0 && s.SpentUSD >= s.BudgetUSD:
		return GateBudget
	case s.Listeners == 0:
		return GateNoListeners
	default:
		return GateOK
	}
}
