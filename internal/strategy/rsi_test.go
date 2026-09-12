package strategy

import (
	"testing"
	"time"
)

// day builds a dailyClose for a calendar date, keeping the tests readable.
func day(y int, m time.Month, d int, close float64) dailyClose {
	return dailyClose{Date: time.Date(y, m, d, 0, 0, 0, 0, time.UTC), Close: close}
}

func TestResampleClosesWeekly(t *testing.T) {
	// 2024-01-01 is a Monday, so each Mon–Fri block is one ISO week.
	series := []dailyClose{
		day(2024, time.January, 1, 10), // ISO week 1
		day(2024, time.January, 3, 11),
		day(2024, time.January, 5, 12), // week 1 closes here
		day(2024, time.January, 8, 13), // ISO week 2
		day(2024, time.January, 12, 15),
	}

	got := resampleCloses(series, weekBucket)
	want := []float64{12, 15}

	if len(got) != len(want) {
		t.Fatalf("resampleCloses weekly: got %d closes %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("resampleCloses weekly [%d]: got %v, want %v", i, got[i], want[i])
		}
	}
}

// A week spanning a year boundary must stay one bucket — that is precisely what
// ISO week numbering buys us over (year, week-of-year).
func TestResampleClosesWeeklyAcrossYearBoundary(t *testing.T) {
	series := []dailyClose{
		day(2024, time.December, 30, 50), // Mon, ISO week 1 of 2025
		day(2025, time.January, 2, 55),   // Thu, same ISO week
		day(2025, time.January, 6, 60),   // Mon, next ISO week
	}

	got := resampleCloses(series, weekBucket)
	if len(got) != 2 {
		t.Fatalf("expected 2 weekly closes across the year boundary, got %d: %v", len(got), got)
	}
	if got[0] != 55 || got[1] != 60 {
		t.Errorf("got %v, want [55 60]", got)
	}
}

func TestResampleClosesMonthly(t *testing.T) {
	series := []dailyClose{
		day(2024, time.January, 2, 18),
		day(2024, time.January, 31, 20), // January closes here
		day(2024, time.February, 1, 21),
		day(2024, time.February, 29, 25), // February closes here
		day(2024, time.March, 1, 26),     // in-progress month
	}

	got := resampleCloses(series, monthBucket)
	want := []float64{20, 25, 26}

	if len(got) != len(want) {
		t.Fatalf("resampleCloses monthly: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("resampleCloses monthly [%d]: got %v, want %v", i, got[i], want[i])
		}
	}
}

func TestResampleClosesEmpty(t *testing.T) {
	if got := resampleCloses(nil, weekBucket); got != nil {
		t.Errorf("expected nil for an empty series, got %v", got)
	}
}

func TestRSIFromCloses(t *testing.T) {
	rising := make([]float64, 0, 20)
	falling := make([]float64, 0, 20)
	for i := 0; i < 20; i++ {
		rising = append(rising, float64(100+i))
		falling = append(falling, float64(100-i))
	}

	if got := rsiFromCloses(rising, 14); got != 100 {
		t.Errorf("an unbroken advance should read 100, got %v", got)
	}
	if got := rsiFromCloses(falling, 14); got != 0 {
		t.Errorf("an unbroken decline should read 0, got %v", got)
	}

	// Too short to seed the average — reported as unknown, not as oversold.
	if got := rsiFromCloses(rising[:14], 14); got != 0 {
		t.Errorf("a series of exactly `period` closes cannot seed an RSI, got %v", got)
	}
}

// The Advanced GFS daily leg: band 57–63, turning up above 60.
func TestAdvancedGFSDailyLeg(t *testing.T) {
	cases := []struct {
		name      string
		prev, rsi float64
		want      bool
	}{
		{"turns up from mid-band", 59, 61, true},
		{"turns up from the band floor", 57, 60.5, true},
		{"turns up from the band ceiling", 63, 64, true},
		{"prev below the band", 56.9, 62, false},
		{"prev above the band", 63.1, 65, false},
		{"today lower than yesterday", 62, 61, false},
		{"today equal to yesterday", 61, 61, false},
		{"rising but still under the level", 57, 58, false},
		{"rising to exactly the level is not above it", 57, 60, false},
		{"unknown prevRSI during warm-up", 0, 62, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := turnedUpAt(tc.prev, tc.rsi, advancedGFSBand); got != tc.want {
				t.Errorf("turnedUpAt(%v, %v, advancedGFSBand) = %v, want %v", tc.prev, tc.rsi, got, tc.want)
			}
		})
	}
}

// The GFS daily leg: band 37–44, turning up above 40.
func TestGFSDailyLeg(t *testing.T) {
	cases := []struct {
		name      string
		prev, rsi float64
		want      bool
	}{
		{"turns up from mid-band", 39, 41, true},
		{"turns up from the band floor", 37, 40.5, true},
		{"turns up from the band ceiling", 44, 45, true},
		{"prev below the band", 36.9, 42, false},
		{"prev above the band", 44.1, 46, false},
		{"today lower than yesterday", 42, 41, false},
		{"today equal to yesterday", 41, 41, false},
		{"rising but still under the level", 37, 39, false},
		{"rising to exactly the level is not above it", 37, 40, false},
		{"unknown prevRSI during warm-up", 0, 42, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := turnedUpAt(tc.prev, tc.rsi, gfsBand); got != tc.want {
				t.Errorf("turnedUpAt(%v, %v, gfsBand) = %v, want %v", tc.prev, tc.rsi, got, tc.want)
			}
		})
	}
}

// The bands must not overlap, otherwise a single bar could satisfy both legs and
// the choice of signal name would come down to evaluation order.
func TestGFSBandsAreDisjoint(t *testing.T) {
	if gfsBand.Upper >= advancedGFSBand.Lower {
		t.Fatalf("bands overlap: GFS upper %v, Advanced GFS lower %v", gfsBand.Upper, advancedGFSBand.Lower)
	}
	for rsi := 0.0; rsi <= 100.0; rsi += 0.5 {
		for prev := 0.0; prev <= 100.0; prev += 0.5 {
			if turnedUpAt(prev, rsi, gfsBand) && turnedUpAt(prev, rsi, advancedGFSBand) {
				t.Fatalf("both legs fired for prev=%v rsi=%v", prev, rsi)
			}
		}
	}
}

// Each leg is memoryless, so it can fire on back-to-back sessions while RSI
// grinds up through its band. That is by design; de-duplication is the caller's
// job (StateRegistry live, -cooldown in the backtester).
func TestDailyLegRepeatsWithinTheBand(t *testing.T) {
	readings := []float64{38, 40.5, 42, 43.5}
	fired := 0
	for i := 1; i < len(readings); i++ {
		if turnedUpAt(readings[i-1], readings[i], gfsBand) {
			fired++
		}
	}
	if fired != 3 {
		t.Errorf("expected the memoryless trigger to fire on all 3 rising bars, got %d", fired)
	}
}

func TestHigherTimeframesBullish(t *testing.T) {
	r := NewRSI(14)

	// Two years of daily closes climbing steadily: every timeframe is maxed out.
	var strong []dailyClose
	d := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 500; i++ {
		strong = append(strong, dailyClose{Date: d, Close: 100 + float64(i)})
		d = d.AddDate(0, 0, 1)
	}
	weekly, monthly, ok := r.higherTimeframesBullish(strong)
	if !ok {
		t.Errorf("a sustained advance should clear both timeframes; weekly=%v monthly=%v", weekly, monthly)
	}

	// A short series cannot produce a monthly RSI, and an unknown higher
	// timeframe must never be read as confirmation.
	weekly, monthly, ok = r.higherTimeframesBullish(strong[:40])
	if ok {
		t.Errorf("40 sessions cannot confirm a monthly RSI; weekly=%v monthly=%v", weekly, monthly)
	}
	if monthly != 0 {
		t.Errorf("expected an unknown (0) monthly RSI from 40 sessions, got %v", monthly)
	}
}
