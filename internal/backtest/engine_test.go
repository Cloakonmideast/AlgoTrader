package backtest

import (
	"math"
	"testing"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
	kitemodels "github.com/zerodha/gokiteconnect/v4/models"
)

// bars builds a candle series from closes, with highs and lows straddling each
// close so excursion tracking has something to read.
func bars(closes ...float64) []kiteconnect.HistoricalData {
	out := make([]kiteconnect.HistoricalData, len(closes))
	day := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, c := range closes {
		out[i] = kiteconnect.HistoricalData{
			Date:  kitemodels.Time{Time: day.AddDate(0, 0, i)},
			Open:  c,
			High:  c * 1.01,
			Low:   c * 0.99,
			Close: c,
		}
	}
	return out
}

func TestSMACloses(t *testing.T) {
	sma := smaCloses(bars(10, 20, 30, 40, 50), 3)

	if len(sma) != 5 {
		t.Fatalf("expected one SMA slot per candle, got %d", len(sma))
	}
	// The first period-1 slots have too little history to average.
	for i := 0; i < 2; i++ {
		if !math.IsNaN(sma[i]) {
			t.Errorf("sma[%d] = %v, want NaN before the window fills", i, sma[i])
		}
	}
	for i, want := range map[int]float64{2: 20, 3: 30, 4: 40} {
		if math.Abs(sma[i]-want) > 1e-9 {
			t.Errorf("sma[%d] = %v, want %v", i, sma[i], want)
		}
	}
}

// The rolling window must not drift as it slides — a naive running sum that
// forgets to subtract the departing bar passes the early indices and fails here.
func TestSMAClosesSlidesCorrectly(t *testing.T) {
	closes := []float64{5, 5, 5, 5, 5, 100, 5, 5, 5, 5, 5}
	sma := smaCloses(bars(closes...), 3)

	// Once the 100 has left the window the average returns to exactly 5.
	if got := sma[len(sma)-1]; math.Abs(got-5) > 1e-9 {
		t.Errorf("SMA should return to 5 after the spike leaves the window, got %v", got)
	}
}

func TestSMAClosesDisabled(t *testing.T) {
	if got := smaCloses(bars(1, 2, 3), 0); got != nil {
		t.Errorf("period 0 must yield no series, got %v", got)
	}
}

func TestMeasureLegNoStopRunsFullHorizon(t *testing.T) {
	c := bars(100, 110, 90, 120)
	leg := measureLeg(c, 0, 3, 100, Long, 0, stopRule{})

	if !leg.Valid {
		t.Fatal("leg should be valid")
	}
	if leg.Stopped {
		t.Error("no stop was configured, so the leg must not report one")
	}
	if leg.HeldDays != 3 {
		t.Errorf("HeldDays = %d, want the full 3", leg.HeldDays)
	}
	if math.Abs(leg.ReturnPct-20) > 1e-9 {
		t.Errorf("ReturnPct = %v, want +20", leg.ReturnPct)
	}
}

// A long is cut on the first CLOSE below the average, and the return is measured
// to that close rather than to the horizon's.
func TestMeasureLegStopsLongOnCloseBelowSMA(t *testing.T) {
	// Rises, then breaks down hard on index 5 and recovers by the horizon end.
	c := bars(10, 11, 12, 13, 14, 5, 30, 40)
	sma := smaCloses(c, 3)

	leg := measureLeg(c, 4, 3, 14, Long, 0, stopRule{SMA: sma})
	if !leg.Stopped {
		t.Fatal("a close of 5 against a rising average must stop the long")
	}
	if leg.HeldDays != 1 {
		t.Errorf("HeldDays = %d, want 1 — the stop hit the very next session", leg.HeldDays)
	}
	if leg.ExitPrice != 5 {
		t.Errorf("ExitPrice = %v, want the stopping close of 5", leg.ExitPrice)
	}
	if leg.ReturnPct >= 0 {
		t.Errorf("ReturnPct = %v, want a loss", leg.ReturnPct)
	}

	// Without the stop the same trade rides the recovery to +185%.
	unstopped := measureLeg(c, 4, 3, 14, Long, 0, stopRule{})
	if unstopped.Stopped || unstopped.ReturnPct <= 0 {
		t.Errorf("unstopped leg should have run to a profit, got %+v", unstopped)
	}
}

// The mirror: a short is stopped by a close ABOVE the average.
func TestMeasureLegStopsShortOnCloseAboveSMA(t *testing.T) {
	c := bars(20, 19, 18, 17, 16, 40, 5, 4)
	sma := smaCloses(c, 3)

	leg := measureLeg(c, 4, 3, 16, Short, 0, stopRule{SMA: sma})
	if !leg.Stopped {
		t.Fatal("a close of 40 against a falling average must stop the short")
	}
	if leg.ExitPrice != 40 {
		t.Errorf("ExitPrice = %v, want 40", leg.ExitPrice)
	}
	if leg.ReturnPct >= 0 {
		t.Errorf("a short stopped on a spike up must lose, got %v", leg.ReturnPct)
	}
}

// The stop is checked from the session AFTER entry, so a signal that fires while
// price is already under its average is not stopped out at zero holding period.
func TestMeasureLegNeverStopsOnTheEntryBar(t *testing.T) {
	// Index 3 closes at 1, far below its own 3-bar average.
	c := bars(10, 10, 10, 1, 12, 13)
	sma := smaCloses(c, 3)

	leg := measureLeg(c, 3, 2, 1, Long, 0, stopRule{SMA: sma})
	if leg.HeldDays == 0 {
		t.Fatal("a leg must never be stopped on the bar it was entered")
	}
	if leg.ReturnPct <= 0 {
		t.Errorf("entering at 1 and running to 13 should profit, got %v", leg.ReturnPct)
	}
}

// An undefined average early in the series must be skipped, not treated as a
// stop trigger — NaN comparisons are false, which would silently never stop.
func TestMeasureLegSkipsUndefinedSMA(t *testing.T) {
	c := bars(10, 9, 8, 7, 6, 5)
	sma := smaCloses(c, 4)

	leg := measureLeg(c, 0, 2, 10, Long, 0, stopRule{SMA: sma})
	if !leg.Valid {
		t.Fatal("leg should still be valid while the average is warming up")
	}
	if leg.Stopped {
		t.Error("a NaN average must not count as a stop trigger")
	}
}

// ── Trailing stop ─────────────────────────────────────────────────────────────

// ohlc builds a series with explicit highs and lows, which the trailing stop
// needs — the armed level is a low, and the trigger reads highs and lows.
type ohlc struct{ o, h, l, c float64 }

func candlesOf(bs ...ohlc) []kiteconnect.HistoricalData {
	out := make([]kiteconnect.HistoricalData, len(bs))
	day := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, b := range bs {
		out[i] = kiteconnect.HistoricalData{
			Date: kitemodels.Time{Time: day.AddDate(0, 0, i)},
			Open: b.o, High: b.h, Low: b.l, Close: b.c,
		}
	}
	return out
}

// flatSMA pins the average at a constant so the tests exercise the stop logic
// rather than the average's warm-up.
func flatSMA(n int, level float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = level
	}
	return out
}

// The whole point of the rule: closing under the average ARMS a level, it does
// not exit. The trade must survive the very session that armed it, even though
// that session's own low is by definition equal to the level.
func TestTrailingStopArmsWithoutExiting(t *testing.T) {
	c := candlesOf(
		ohlc{100, 100, 100, 100}, // 0 entry
		ohlc{99, 100, 95, 96},    // 1 closes below 100 → arm at low 95
		ohlc{97, 105, 96, 104},   // 2 holds above 95
		ohlc{104, 110, 103, 109}, // 3
		ohlc{109, 113, 108, 112}, // 4 horizon
	)
	stop := stopRule{SMA: flatSMA(len(c), 100), TrailLow: true}

	leg := measureLeg(c, 0, 4, 100, Long, 0, stop)
	if leg.Stopped {
		t.Fatalf("the armed session must not stop the trade itself, exited at %v", leg.ExitPrice)
	}
	if math.Abs(leg.ReturnPct-12) > 1e-9 {
		t.Errorf("ReturnPct = %v, want +12 from running to the horizon", leg.ReturnPct)
	}
}

func TestTrailingStopExitsOnCloseBelowArmedLow(t *testing.T) {
	c := candlesOf(
		ohlc{100, 100, 100, 100}, // 0 entry
		ohlc{99, 100, 95, 96},    // 1 arm at 95
		ohlc{96, 97, 93, 94},     // 2 closes 94, below 95 → exit
		ohlc{94, 200, 94, 200},   // 3 the rip it must NOT capture
	)
	stop := stopRule{SMA: flatSMA(len(c), 100), TrailLow: true}

	leg := measureLeg(c, 0, 3, 100, Long, 0, stop)
	if !leg.Stopped {
		t.Fatal("a close below the armed low must stop the trade")
	}
	if leg.ExitPrice != 94 {
		t.Errorf("ExitPrice = %v, want the breaking close of 94", leg.ExitPrice)
	}
	if leg.HeldDays != 2 {
		t.Errorf("HeldDays = %d, want 2", leg.HeldDays)
	}
}

func TestTrailingStopIntradayFillsAtTheLevel(t *testing.T) {
	c := candlesOf(
		ohlc{100, 100, 100, 100}, // 0 entry
		ohlc{99, 100, 95, 96},    // 1 arm at 95
		ohlc{97, 105, 90, 104},   // 2 dips to 90 but closes 104
	)
	stop := stopRule{SMA: flatSMA(len(c), 100), TrailLow: true, Intraday: true}

	leg := measureLeg(c, 0, 2, 100, Long, 0, stop)
	if !leg.Stopped {
		t.Fatal("an intraday trade through the level must trigger the stop")
	}
	if leg.ExitPrice != 95 {
		t.Errorf("ExitPrice = %v, want the stop level 95", leg.ExitPrice)
	}

	// On a closing basis the same session does not trigger at all.
	onClose := measureLeg(c, 0, 2, 100, Long, 0, stopRule{SMA: flatSMA(len(c), 100), TrailLow: true})
	if onClose.Stopped {
		t.Error("closing basis must ignore an intraday dip that recovers by the close")
	}
}

// A session that opens below the level never offered it, so the fill is the open.
// Assuming the stop price here would flatter every gap-down in the sample.
func TestTrailingStopGapFillsAtTheOpen(t *testing.T) {
	c := candlesOf(
		ohlc{100, 100, 100, 100}, // 0 entry
		ohlc{99, 100, 95, 96},    // 1 arm at 95
		ohlc{90, 96, 88, 94},     // 2 gaps open to 90, straight through 95
	)
	stop := stopRule{SMA: flatSMA(len(c), 100), TrailLow: true, Intraday: true}

	leg := measureLeg(c, 0, 2, 100, Long, 0, stop)
	if leg.ExitPrice != 90 {
		t.Errorf("ExitPrice = %v, want the gapped open of 90, not the level", leg.ExitPrice)
	}
}

func TestTrailingStopTightens(t *testing.T) {
	c := candlesOf(
		ohlc{100, 100, 100, 100}, // 0 entry
		ohlc{99, 100, 90, 96},    // 1 arm at 90
		ohlc{96, 99, 95, 97},     // 2 closes below 100 again → tighten to 95
		ohlc{97, 98, 92, 93},     // 3 closes 93: below 95, above the old 90
		ohlc{93, 200, 93, 200},   // 4
	)
	stop := stopRule{SMA: flatSMA(len(c), 100), TrailLow: true}

	leg := measureLeg(c, 0, 4, 100, Long, 0, stop)
	if !leg.Stopped || leg.ExitPrice != 93 {
		t.Errorf("the level should have tightened to 95 and stopped at 93, got stopped=%v exit=%v",
			leg.Stopped, leg.ExitPrice)
	}
}

// The mirror case, and the one that matters most: a later low BELOW the armed
// level must not replace it. Loosening would let a trade that already survived a
// tighter stop keep running.
func TestTrailingStopNeverLoosens(t *testing.T) {
	c := candlesOf(
		ohlc{100, 100, 100, 100}, // 0 entry
		ohlc{99, 100, 95, 96},    // 1 arm at 95
		ohlc{96, 99, 90, 97},     // 2 closes below 100, low 90 — must NOT loosen to 90
		ohlc{97, 98, 92, 93},     // 3 closes 93: under 95, over 90
		ohlc{93, 200, 93, 200},   // 4
	)
	stop := stopRule{SMA: flatSMA(len(c), 100), TrailLow: true}

	leg := measureLeg(c, 0, 4, 100, Long, 0, stop)
	if !leg.Stopped {
		t.Fatal("the level loosened to 90 — a close of 93 should have stopped the trade at 95")
	}
	if leg.ExitPrice != 93 {
		t.Errorf("ExitPrice = %v, want 93", leg.ExitPrice)
	}
}

// Short is the mirror: a close ABOVE the average arms at that session's HIGH,
// and the trade exits when a later session takes that high out.
func TestTrailingStopShortMirror(t *testing.T) {
	c := candlesOf(
		ohlc{100, 100, 100, 100}, // 0 entry short
		ohlc{101, 105, 100, 104}, // 1 closes above 100 → arm at high 105
		ohlc{104, 104, 100, 102}, // 2 holds under 105
		ohlc{102, 108, 102, 107}, // 3 closes 107, above 105 → exit
	)
	stop := stopRule{SMA: flatSMA(len(c), 100), TrailLow: true}

	leg := measureLeg(c, 0, 3, 100, Short, 0, stop)
	if !leg.Stopped {
		t.Fatal("a close above the armed high must stop the short")
	}
	if leg.ExitPrice != 107 {
		t.Errorf("ExitPrice = %v, want 107", leg.ExitPrice)
	}
	if leg.ReturnPct >= 0 {
		t.Errorf("a short exited above its entry must lose, got %v", leg.ReturnPct)
	}
}

// A trade that never closes through the average has no stop armed at all and
// runs to the horizon regardless of how deep its intraday lows go.
func TestTrailingStopNeverArmed(t *testing.T) {
	c := candlesOf(
		ohlc{100, 100, 100, 100}, // 0 entry
		ohlc{101, 110, 80, 105},  // 1 wild low, but closes above the average
		ohlc{105, 115, 104, 112}, // 2
	)
	stop := stopRule{SMA: flatSMA(len(c), 100), TrailLow: true, Intraday: true}

	leg := measureLeg(c, 0, 2, 100, Long, 0, stop)
	if leg.Stopped {
		t.Error("no close through the average means no armed level, so no stop")
	}
	if math.Abs(leg.ReturnPct-12) > 1e-9 {
		t.Errorf("ReturnPct = %v, want +12", leg.ReturnPct)
	}
}

// ── Hard max-loss floor ───────────────────────────────────────────────────────

// The floor is a resting order, so it fills intraday even on a session that
// recovers to close well above it.
func TestHardStopFillsIntraday(t *testing.T) {
	c := candlesOf(
		ohlc{100, 100, 100, 100}, // 0 entry
		ohlc{99, 101, 88, 100},   // 1 dips to 88, closes back at 100
	)
	leg := measureLeg(c, 0, 1, 100, Long, 0, stopRule{MaxLossPct: 10})

	if !leg.Stopped {
		t.Fatal("a 10% floor sits at 90; a low of 88 must fill it")
	}
	if math.Abs(leg.ExitPrice-90) > 1e-9 {
		t.Errorf("ExitPrice = %v, want the resting level 90", leg.ExitPrice)
	}
	if math.Abs(leg.ReturnPct-(-10)) > 1e-9 {
		t.Errorf("ReturnPct = %v, want exactly -10", leg.ReturnPct)
	}
}

// The floor caps the loss at its level, but only when the level actually trades.
// A gap straight through it fills at the open and loses more.
func TestHardStopGapsThrough(t *testing.T) {
	c := candlesOf(
		ohlc{100, 100, 100, 100}, // 0 entry
		ohlc{80, 82, 75, 78},     // 1 opens at 80, far below the 90 floor
	)
	leg := measureLeg(c, 0, 1, 100, Long, 0, stopRule{MaxLossPct: 10})

	if math.Abs(leg.ExitPrice-80) > 1e-9 {
		t.Errorf("ExitPrice = %v, want the gapped open of 80", leg.ExitPrice)
	}
	if leg.ReturnPct >= -10 {
		t.Errorf("a gap through the floor must lose MORE than the floor, got %v", leg.ReturnPct)
	}
}

// Shorts rest their floor above entry.
func TestHardStopShort(t *testing.T) {
	c := candlesOf(
		ohlc{100, 100, 100, 100},
		ohlc{101, 115, 100, 103}, // spikes to 115, floor at 110
	)
	leg := measureLeg(c, 0, 1, 100, Short, 0, stopRule{MaxLossPct: 10})

	if !leg.Stopped || math.Abs(leg.ExitPrice-110) > 1e-9 {
		t.Errorf("short floor should rest at 110, got stopped=%v exit=%v", leg.Stopped, leg.ExitPrice)
	}
	if math.Abs(leg.ReturnPct-(-10)) > 1e-9 {
		t.Errorf("ReturnPct = %v, want -10", leg.ReturnPct)
	}
}

// This is the case the floor exists for: a violent move where the SMA rule
// cannot arm in time. Without the floor the trade rides to the close.
func TestHardStopCatchesWhatTheTrailCannot(t *testing.T) {
	c := candlesOf(
		ohlc{100, 100, 100, 100}, // 0 entry short at 100, SMA pinned at 115
		ohlc{101, 140, 101, 138}, // 1 rips 38% — still below the 115 SMA? no: closes above
		ohlc{140, 150, 138, 148}, // 2
	)
	sma := flatSMA(len(c), 115)

	trailOnly := measureLeg(c, 0, 2, 100, Short, 0, stopRule{SMA: sma, TrailLow: true})
	withFloor := measureLeg(c, 0, 2, 100, Short, 0, stopRule{SMA: sma, TrailLow: true, MaxLossPct: 10})

	if withFloor.ReturnPct <= trailOnly.ReturnPct {
		t.Errorf("the floor must cut the loss short: floor %v vs trail-only %v",
			withFloor.ReturnPct, trailOnly.ReturnPct)
	}
	if math.Abs(withFloor.ReturnPct-(-10)) > 1e-9 {
		t.Errorf("the floor should cap this at -10, got %v", withFloor.ReturnPct)
	}
}

// When both a trailing level and the floor are live, price reaches the nearer
// one first — so a tightened trail must pre-empt a floor further away.
func TestTrailPreemptsTheFloorWhenNearer(t *testing.T) {
	c := candlesOf(
		ohlc{100, 100, 100, 100}, // 0 entry long, floor at 90
		ohlc{99, 100, 95, 96},    // 1 closes below the 100 SMA → arm at 95
		ohlc{96, 97, 85, 86},     // 2 breaks both 95 and 90
	)
	stop := stopRule{SMA: flatSMA(len(c), 100), TrailLow: true, Intraday: true, MaxLossPct: 10}

	leg := measureLeg(c, 0, 2, 100, Long, 0, stop)
	if math.Abs(leg.ExitPrice-95) > 1e-9 {
		t.Errorf("ExitPrice = %v, want the nearer trailing level 95, not the 90 floor", leg.ExitPrice)
	}
}

func TestFirstTouched(t *testing.T) {
	nan := math.NaN()
	if got := firstTouched(nan, 95, Long); got != 95 {
		t.Errorf("a NaN operand must yield the other level, got %v", got)
	}
	if got := firstTouched(90, nan, Short); got != 90 {
		t.Errorf("a NaN operand must yield the other level, got %v", got)
	}
	if got := firstTouched(90, 95, Long); got != 95 {
		t.Errorf("a falling long reaches the higher level first, got %v", got)
	}
	if got := firstTouched(105, 110, Short); got != 105 {
		t.Errorf("a rising short reaches the lower level first, got %v", got)
	}
	if got := firstTouched(nan, nan, Long); !math.IsNaN(got) {
		t.Errorf("two unset levels must stay unset, got %v", got)
	}
}

// A floor set wider than anything the trade ever suffers must not alter the
// outcome at all.
func TestHardStopInactiveWhenWide(t *testing.T) {
	c := bars(100, 102, 104, 106)
	plain := measureLeg(c, 0, 3, 100, Long, 0, stopRule{})
	floored := measureLeg(c, 0, 3, 100, Long, 0, stopRule{MaxLossPct: 50})

	if floored.Stopped || math.Abs(floored.ReturnPct-plain.ReturnPct) > 1e-9 {
		t.Errorf("a 50%% floor should be inert here: %v vs %v", floored.ReturnPct, plain.ReturnPct)
	}
}

// ── Truncated horizons at the edge of the data ────────────────────────────────

// A trade whose stop fires before the data runs out has a known outcome, even
// though the full horizon does not fit. Discarding it would throw away every
// recent signal.
func TestStoppedLegCountsBeyondAvailableHorizon(t *testing.T) {
	c := candlesOf(
		ohlc{100, 100, 100, 100}, // 0 entry
		ohlc{99, 100, 95, 96},    // 1 arm at 95
		ohlc{96, 97, 93, 94},     // 2 exit at 94 — and the series ends here
	)
	stop := stopRule{SMA: flatSMA(len(c), 100), TrailLow: true}

	leg := measureLeg(c, 0, 126, 100, Long, 0, stop) // horizon far past the data
	if !leg.Valid {
		t.Fatal("a trade that stopped inside the data must count")
	}
	if !leg.Stopped || leg.ExitPrice != 94 {
		t.Errorf("expected a stop at 94, got stopped=%v exit=%v", leg.Stopped, leg.ExitPrice)
	}
}

// The mirror: a trade still open where the data ends has no known outcome and
// must be excluded rather than marked to the last close.
func TestOpenLegAtDataEdgeIsExcluded(t *testing.T) {
	c := candlesOf(
		ohlc{100, 100, 100, 100},
		ohlc{101, 106, 100, 105}, // never closes below the average
		ohlc{105, 112, 104, 110},
	)
	stop := stopRule{SMA: flatSMA(len(c), 100), TrailLow: true}

	if leg := measureLeg(c, 0, 126, 100, Long, 0, stop); leg.Valid {
		t.Errorf("a still-open trade must not be scored, got %+v", leg)
	}
}

// Without a stop the horizon is the only exit, so it still has to fit.
func TestUnstoppedLegStillNeedsTheFullHorizon(t *testing.T) {
	c := bars(100, 101, 102)
	if leg := measureLeg(c, 0, 126, 100, Long, 0, stopRule{}); leg.Valid {
		t.Error("with no stop configured, a horizon past the data must be invalid")
	}
}

func TestMeasureLegStopRespectsCost(t *testing.T) {
	c := bars(100, 100, 100, 100)
	withCost := measureLeg(c, 0, 3, 100, Long, 20, stopRule{})
	if math.Abs(withCost.ReturnPct-(-0.20)) > 1e-9 {
		t.Errorf("20 bps on a flat trade should read -0.20%%, got %v", withCost.ReturnPct)
	}
}
