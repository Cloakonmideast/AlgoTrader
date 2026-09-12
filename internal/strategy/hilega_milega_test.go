package strategy

import (
	"math"
	"testing"
)

func approx(t *testing.T, got, want float64, tol float64, label string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s: got %v, want %v (tolerance %v)", label, got, want, tol)
	}
}

// rsiSeries must agree with rsiFromCloses at every prefix, since one is just the
// series form of the other.
func TestRSISeriesMatchesRSIFromCloses(t *testing.T) {
	prices := []float64{
		44.34, 44.09, 44.15, 43.61, 44.33, 44.83, 45.10, 45.42, 45.84,
		46.08, 45.89, 46.03, 45.61, 46.28, 46.28, 46.00, 46.03, 46.41,
		46.22, 45.64, 46.21, 46.25, 45.71, 46.45, 45.78, 45.35, 44.03,
	}
	series := rsiSeries(prices, 14)
	if len(series) != len(prices)-14 {
		t.Fatalf("expected %d RSI values, got %d", len(prices)-14, len(series))
	}

	for i := range series {
		prefix := prices[:14+i+1]
		want := rsiFromCloses(prefix, 14)
		approx(t, series[i], want, 1e-9, "rsiSeries vs rsiFromCloses at index")
	}
}

// A textbook Wilder RSI check against the classic worked example.
func TestRSISeriesKnownValue(t *testing.T) {
	prices := []float64{
		44.34, 44.09, 44.15, 43.61, 44.33, 44.83, 45.10, 45.42, 45.84,
		46.08, 45.89, 46.03, 45.61, 46.28, 46.28,
	}
	series := rsiSeries(prices, 14)
	if len(series) != 1 {
		t.Fatalf("expected exactly 1 RSI value, got %d", len(series))
	}
	// Wilder's own example yields ~70.5 at this point.
	approx(t, series[0], 70.53, 0.1, "Wilder worked example")
}

func TestRSISeriesTooShort(t *testing.T) {
	if got := rsiSeries([]float64{1, 2, 3}, 14); got != nil {
		t.Errorf("a series shorter than the period must yield nil, got %v", got)
	}
}

// The EMA seeds on the SMA of the first `period` values, matching Pine's ta.ema,
// and each later value applies the 2/(n+1) smoothing factor.
func TestEMASeries(t *testing.T) {
	values := []float64{10, 20, 30, 40, 50}
	got := emaSeries(values, 3)

	if len(got) != 3 {
		t.Fatalf("expected 3 EMA values from 5 inputs with period 3, got %d: %v", len(got), got)
	}
	approx(t, got[0], 20, 1e-9, "EMA seed = SMA(10,20,30)")
	// k = 0.5 → 40*0.5 + 20*0.5 = 30
	approx(t, got[1], 30, 1e-9, "EMA second value")
	// 50*0.5 + 30*0.5 = 40
	approx(t, got[2], 40, 1e-9, "EMA third value")
}

// A flat input must produce a flat EMA — a cheap guard against a broken seed.
func TestEMASeriesFlatInput(t *testing.T) {
	values := []float64{7, 7, 7, 7, 7, 7}
	for i, v := range emaSeries(values, 3) {
		approx(t, v, 7, 1e-9, "flat EMA")
		_ = i
	}
}

// WMA weights the most recent value by `period` and the oldest by 1.
func TestWMASeries(t *testing.T) {
	values := []float64{1, 2, 3, 4}
	got := wmaSeries(values, 3)

	if len(got) != 2 {
		t.Fatalf("expected 2 WMA values from 4 inputs with period 3, got %d: %v", len(got), got)
	}
	// (1*1 + 2*2 + 3*3) / 6 = 14/6
	approx(t, got[0], 14.0/6.0, 1e-9, "WMA first window")
	// (2*1 + 3*2 + 4*3) / 6 = 20/6
	approx(t, got[1], 20.0/6.0, 1e-9, "WMA second window")
}

// The WMA must lag a rising series — it sits below the newest value.
func TestWMASeriesLagsARise(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5, 6}
	got := wmaSeries(values, 3)
	last := got[len(got)-1]
	if last >= values[len(values)-1] {
		t.Errorf("a WMA of a rising series must lag it: WMA %v vs latest %v", last, values[len(values)-1])
	}
}

func TestMovingAveragesTooShort(t *testing.T) {
	if got := emaSeries([]float64{1, 2}, 3); got != nil {
		t.Errorf("EMA of a too-short series must be nil, got %v", got)
	}
	if got := wmaSeries([]float64{1, 2}, 3); got != nil {
		t.Errorf("WMA of a too-short series must be nil, got %v", got)
	}
}

// Both averages tail the series, so their last elements describe the same bar.
// If that ever stops holding, the strategy silently compares mismatched bars.
func TestMovingAveragesAreTailAligned(t *testing.T) {
	values := make([]float64, 40)
	for i := range values {
		values[i] = float64(i)
	}
	ema := emaSeries(values, 3)
	wma := wmaSeries(values, 21)

	// Extending the input by one bar must extend each output by exactly one,
	// which is what makes indexing from the tail safe.
	ema2 := emaSeries(append(values, 40), 3)
	wma2 := wmaSeries(append(values, 40), 21)

	if len(ema2) != len(ema)+1 {
		t.Errorf("EMA length should grow by 1 per extra bar: %d → %d", len(ema), len(ema2))
	}
	if len(wma2) != len(wma)+1 {
		t.Errorf("WMA length should grow by 1 per extra bar: %d → %d", len(wma), len(wma2))
	}
}

func TestClassifyTiers(t *testing.T) {
	h := NewHilegaMilega()

	cases := []struct {
		name string
		bar  hmBar
		want hmTier
	}{
		// ── BUY ───────────────────────────────────────────────────────────────
		{"buy-2: stacked, RSI 60, WMA 52 rising", hmBar{RSI: 60, EMA: 56, WMA: 52, PrevWMA: 51}, hmBuy2},
		{"buy-1: same stack but WMA below 50", hmBar{RSI: 60, EMA: 50, WMA: 48, PrevWMA: 47}, hmBuy1},
		{"buy-1: WMA exactly at 50 is not above it", hmBar{RSI: 60, EMA: 55, WMA: 50, PrevWMA: 49}, hmBuy1},
		{"no buy: RSI 55 does not clear 55", hmBar{RSI: 55, EMA: 52, WMA: 50, PrevWMA: 49}, hmNone},
		{"no buy: WMA flat, not rising", hmBar{RSI: 60, EMA: 56, WMA: 52, PrevWMA: 52}, hmNone},
		{"no buy: WMA falling", hmBar{RSI: 60, EMA: 56, WMA: 52, PrevWMA: 53}, hmNone},
		{"no buy: EMA above RSI", hmBar{RSI: 60, EMA: 62, WMA: 52, PrevWMA: 51}, hmNone},
		{"no buy: WMA above EMA breaks the stack", hmBar{RSI: 60, EMA: 52, WMA: 56, PrevWMA: 55}, hmNone},

		// ── SELL ──────────────────────────────────────────────────────────────
		{"sell-2: stacked, RSI 40, WMA 45 falling", hmBar{RSI: 40, EMA: 43, WMA: 45, PrevWMA: 46}, hmSell2},
		{"sell-1: same stack but WMA above 50", hmBar{RSI: 40, EMA: 48, WMA: 52, PrevWMA: 53}, hmSell1},
		{"sell-1: WMA exactly at 50 is not below it", hmBar{RSI: 40, EMA: 45, WMA: 50, PrevWMA: 51}, hmSell1},
		{"no sell: RSI 48 does not clear 48", hmBar{RSI: 48, EMA: 50, WMA: 52, PrevWMA: 53}, hmNone},
		{"no sell: WMA rising", hmBar{RSI: 40, EMA: 43, WMA: 45, PrevWMA: 44}, hmNone},
		{"no sell: EMA below RSI", hmBar{RSI: 40, EMA: 38, WMA: 45, PrevWMA: 46}, hmNone},

		// ── Dead band ─────────────────────────────────────────────────────────
		{"dead band: RSI 50 stacked bullish", hmBar{RSI: 50, EMA: 46, WMA: 42, PrevWMA: 41}, hmNone},
		{"dead band: RSI 52 stacked bearish", hmBar{RSI: 52, EMA: 56, WMA: 60, PrevWMA: 61}, hmNone},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.classify(tc.bar); got != tc.want {
				t.Errorf("classify(%+v) = %v, want %v", tc.bar, got, tc.want)
			}
		})
	}
}

// Tier 2 is a strict refinement of tier 1, so anything classified BUY-2 must
// also satisfy every BUY-1 condition. Brute-forced over a coarse grid.
func TestTier2ImpliesTier1(t *testing.T) {
	h := NewHilegaMilega()

	buy1 := func(b hmBar) bool {
		return b.RSI > h.BuyRSI && b.EMA < b.RSI && b.WMA < b.EMA && b.WMA > b.PrevWMA
	}
	sell1 := func(b hmBar) bool {
		return b.RSI < h.SellRSI && b.EMA > b.RSI && b.WMA > b.EMA && b.WMA < b.PrevWMA
	}

	for rsi := 0.0; rsi <= 100; rsi += 5 {
		for ema := 0.0; ema <= 100; ema += 5 {
			for wma := 0.0; wma <= 100; wma += 5 {
				for _, prev := range []float64{wma - 1, wma, wma + 1} {
					b := hmBar{RSI: rsi, EMA: ema, WMA: wma, PrevWMA: prev}
					switch h.classify(b) {
					case hmBuy2:
						if !buy1(b) {
							t.Fatalf("%+v classified BUY-2 without satisfying BUY-1", b)
						}
					case hmSell2:
						if !sell1(b) {
							t.Fatalf("%+v classified SELL-2 without satisfying SELL-1", b)
						}
					}
				}
			}
		}
	}
}

// The 48–55 dead band means no bar can ever satisfy a buy and a sell tier at
// once — the two sides are structurally exclusive, not merely unlikely.
func TestBuyAndSellCannotOverlap(t *testing.T) {
	h := NewHilegaMilega()
	if h.SellRSI >= h.BuyRSI {
		t.Fatalf("sell threshold %v must sit below the buy threshold %v", h.SellRSI, h.BuyRSI)
	}
}

func TestFiredOnRisingEdgeOnly(t *testing.T) {
	cases := []struct {
		name       string
		cur, prev  hmTier
		wantSignal bool
	}{
		{"fresh buy-1", hmBuy1, hmNone, true},
		{"buy-1 held for a second bar", hmBuy1, hmBuy1, false},
		{"buy-1 tightening into buy-2 is an upgrade", hmBuy2, hmBuy1, true},
		{"buy-2 loosening back to buy-1 re-fires", hmBuy1, hmBuy2, true},
		{"buy-2 held", hmBuy2, hmBuy2, false},
		{"flip straight from buy to sell", hmSell2, hmBuy2, true},
		{"no tier at all", hmNone, hmBuy1, false},
		{"nothing either bar", hmNone, hmNone, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fired(tc.cur, tc.prev); got != tc.wantSignal {
				t.Errorf("fired(%v, %v) = %v, want %v", tc.cur, tc.prev, got, tc.wantSignal)
			}
		})
	}
}

func TestTierNames(t *testing.T) {
	for tier, want := range map[hmTier]string{
		hmNone: "", hmBuy1: "BUY-1", hmBuy2: "BUY-2", hmSell1: "SELL-1", hmSell2: "SELL-2",
	} {
		if got := tier.String(); got != want {
			t.Errorf("tier %d rendered %q, want %q", tier, got, want)
		}
	}
}

// LongOnly must gate only the sell tiers — the buy tiers and the classifier
// itself are untouched, so a long-only instance still scores identically on
// everything it does emit.
func TestLongOnlySuppressesOnlySells(t *testing.T) {
	h := NewHilegaMilega()
	h.LongOnly = true

	if h.classify(hmBar{RSI: 40, EMA: 43, WMA: 45, PrevWMA: 46}) != hmSell2 {
		t.Error("LongOnly must not change classification — it filters at emission")
	}
	if h.classify(hmBar{RSI: 60, EMA: 56, WMA: 52, PrevWMA: 51}) != hmBuy2 {
		t.Error("buy tiers must be unaffected by LongOnly")
	}
	if NewHilegaMilega().LongOnly {
		t.Error("LongOnly must default to false so existing results stay reproducible")
	}
}

func TestHilegaMilegaDefaults(t *testing.T) {
	h := NewHilegaMilega()
	if h.RSIPeriod != 9 || h.EMAPeriod != 3 || h.WMAPeriod != 21 {
		t.Errorf("published periods are RSI 9 / EMA 3 / WMA 21, got %d/%d/%d",
			h.RSIPeriod, h.EMAPeriod, h.WMAPeriod)
	}
	if h.BuyRSI != 55 || h.SellRSI != 48 || h.Level != 50 {
		t.Errorf("thresholds are buy 55 / sell 48 / level 50, got %v/%v/%v",
			h.BuyRSI, h.SellRSI, h.Level)
	}
	if h.Name() != "HilegaMilega" {
		t.Errorf("unexpected strategy name %q", h.Name())
	}
	if got, want := h.MinCandles(), 9+21+3; got != want {
		t.Errorf("MinCandles() = %d, want %d", got, want)
	}
}
