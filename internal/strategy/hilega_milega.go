package strategy

import (
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
	"github.com/zerodha/gokiteconnect/v4/models"

	"algotrader/internal/domain"
)

// HilegaMilega implements the Hilega Milega setup attributed to Nitish Kumar
// ("NK sir"), which plots two moving averages ON the RSI series rather than on
// price:
//
//	RSI(9)              the "strength" line
//	EMA(3)  of RSI(9)   the "price strength" line, fast
//	WMA(21) of RSI(9)   the "volume strength" line, slow
//
// A signal needs the three lines fully stacked in one direction, the RSI clear
// of a directional threshold, and the slow line already moving the right way:
//
//	BUY-1   RSI > 55, RSI > EMA > WMA, WMA rising
//	BUY-2   BUY-1 and WMA above 50
//	SELL-1  RSI < 48, RSI < EMA < WMA, WMA falling
//	SELL-2  SELL-1 and WMA below 50
//
// The tier-2 rules are strict refinements of tier 1 — every BUY-2 bar is also a
// BUY-1 bar — so Evaluate reports only the STRONGEST tier that matches. The four
// signal sets are therefore disjoint, and a backtest can score them separately
// without double-counting.
//
// Signals fire on the bar a tier is established, not on every bar it persists,
// so a trend that holds for weeks produces one signal rather than weeks of them.
// A stock that qualifies for BUY-1 today and BUY-2 tomorrow emits both, because
// the upgrade is itself a rising edge.
//
// Like RSI this strategy is stateless: the decision reads the candle series and
// nothing else, so one instance is safe across all instruments and workers.
type HilegaMilega struct {
	RSIPeriod int
	EMAPeriod int
	WMAPeriod int

	// BuyRSI and SellRSI are the directional thresholds the RSI itself must
	// clear. They are deliberately asymmetric and leave a dead band between 48
	// and 55 where neither side can trigger.
	BuyRSI  float64
	SellRSI float64

	// Level is the zone divider applied to the WMA in the tier-2 rules: the slow
	// line must sit above it to buy and below it to sell.
	Level float64

	// LongOnly suppresses SELL-1 and SELL-2 entirely.
	//
	// The short rules have lost money in eight of the last ten years — roughly
	// -1.2% per trade at 3M with a trailing 20-SMA stop, against +1.6% for the
	// long rules on the same exit — and the only two profitable years were the
	// 2018 and 2020 crashes. They are also the harder side to stop: a SELL fires
	// with price well BELOW its 20-SMA, so a stop that arms on a close back
	// through the average cannot exist until the whole gap is covered.
	LongOnly bool
}

// NewHilegaMilega builds the strategy with the published defaults: RSI 9,
// EMA 3, WMA 21, buy above 55, sell below 48, zone divider 50.
func NewHilegaMilega() *HilegaMilega {
	return &HilegaMilega{
		RSIPeriod: 9,
		EMAPeriod: 3,
		WMAPeriod: 21,
		BuyRSI:    55,
		SellRSI:   48,
		Level:     50,
	}
}

func (h *HilegaMilega) Name() string { return "HilegaMilega" }

// MinCandles is the shortest close series that can produce a signal: enough to
// seed the RSI, then enough RSI values to fill the WMA window, plus the extra
// bars needed to know the previous bar's tier — which itself needs the WMA slope
// one bar further back.
func (h *HilegaMilega) MinCandles() int {
	return h.RSIPeriod + h.WMAPeriod + 3
}

// rsiSeries computes Wilder's RSI at every point it is defined, returning one
// value per input close starting at index `period`. It is the series form of
// rsiFromCloses, which returns only the final value.
func rsiSeries(prices []float64, period int) []float64 {
	if period <= 0 || len(prices) <= period {
		return nil
	}

	var sumGain, sumLoss float64
	for i := 1; i <= period; i++ {
		change := prices[i] - prices[i-1]
		if change > 0 {
			sumGain += change
		} else {
			sumLoss -= change
		}
	}
	avgGain := sumGain / float64(period)
	avgLoss := sumLoss / float64(period)

	out := make([]float64, 0, len(prices)-period)
	out = append(out, rsiFrom(avgGain, avgLoss))

	for i := period + 1; i < len(prices); i++ {
		change := prices[i] - prices[i-1]
		gain, loss := 0.0, 0.0
		if change > 0 {
			gain = change
		} else {
			loss = -change
		}
		avgGain = (avgGain*float64(period-1) + gain) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + loss) / float64(period)
		out = append(out, rsiFrom(avgGain, avgLoss))
	}
	return out
}

// rsiFrom converts a smoothed gain/loss pair into an RSI reading.
func rsiFrom(avgGain, avgLoss float64) float64 {
	if avgLoss == 0 {
		return 100.0
	}
	return 100.0 - (100.0 / (1.0 + avgGain/avgLoss))
}

// emaSeries computes an exponential moving average, seeded with the simple
// average of the first `period` values the way Pine Script's ta.ema does. The
// result is aligned to the tail of values: out[len(out)-1] corresponds to
// values[len(values)-1].
func emaSeries(values []float64, period int) []float64 {
	if period <= 0 || len(values) < period {
		return nil
	}

	var seed float64
	for _, v := range values[:period] {
		seed += v
	}
	prev := seed / float64(period)

	out := make([]float64, 0, len(values)-period+1)
	out = append(out, prev)

	k := 2.0 / (float64(period) + 1.0)
	for _, v := range values[period:] {
		prev = v*k + prev*(1-k)
		out = append(out, prev)
	}
	return out
}

// wmaSeries computes a linearly weighted moving average, where the most recent
// value carries weight `period` and the oldest carries weight 1. The result is
// aligned to the tail of values the same way emaSeries is.
func wmaSeries(values []float64, period int) []float64 {
	if period <= 0 || len(values) < period {
		return nil
	}

	denom := float64(period*(period+1)) / 2.0
	out := make([]float64, 0, len(values)-period+1)

	for end := period; end <= len(values); end++ {
		var num float64
		window := values[end-period : end]
		for i, v := range window {
			num += v * float64(i+1)
		}
		out = append(out, num/denom)
	}
	return out
}

// hmTier names the four signals plus the no-signal case, ordered so a larger
// value is a stricter setup within its own direction.
type hmTier int

const (
	hmNone hmTier = iota
	hmBuy1
	hmBuy2
	hmSell1
	hmSell2
)

func (t hmTier) String() string {
	switch t {
	case hmBuy1:
		return "BUY-1"
	case hmBuy2:
		return "BUY-2"
	case hmSell1:
		return "SELL-1"
	case hmSell2:
		return "SELL-2"
	}
	return ""
}

// hmBar is one bar's worth of the three lines, plus the previous bar's WMA so
// the slope of the slow line is known.
type hmBar struct {
	RSI     float64
	EMA     float64
	WMA     float64
	PrevWMA float64
}

// classify returns the strongest tier the bar satisfies.
//
// Buying wants the fast line to have crossed BELOW the RSI and the slow line
// below that again — strength leading both averages — with the slow line already
// turning up so the move has support rather than being a single spike. Selling
// is the mirror image, with a lower RSI threshold so the two sides do not touch:
// nothing between 48 and 55 can signal at all.
func (h *HilegaMilega) classify(b hmBar) hmTier {
	switch {
	case b.RSI > h.BuyRSI && b.EMA < b.RSI && b.WMA < b.EMA && b.WMA > b.PrevWMA:
		if b.WMA > h.Level {
			return hmBuy2
		}
		return hmBuy1

	case b.RSI < h.SellRSI && b.EMA > b.RSI && b.WMA > b.EMA && b.WMA < b.PrevWMA:
		if b.WMA < h.Level {
			return hmSell2
		}
		return hmSell1
	}
	return hmNone
}

// fired reports whether `cur` is a fresh signal given the previous bar's tier.
// Each tier has its own rising edge, so a BUY-1 that tightens into a BUY-2 the
// next session emits the upgrade rather than staying silent.
func fired(cur, prev hmTier) bool {
	return cur != hmNone && cur != prev
}

// at indexes a series from its tail: at(s, 0) is the last value. Each of the
// three lines has a different length because each average consumes its own
// warm-up window, so tail indexing is what keeps them on the same calendar bar.
func at(s []float64, back int) float64 { return s[len(s)-1-back] }

func (h *HilegaMilega) Evaluate(tick models.Tick, ctx *domain.MarketContext) (bool, domain.OrderSignal) {
	rawCandles, ok := ctx.Get(tick.InstrumentToken)
	if !ok || len(rawCandles) < h.MinCandles() {
		return false, domain.OrderSignal{}
	}

	candles := make([]kiteconnect.HistoricalData, len(rawCandles))
	copy(candles, rawCandles)

	// Strip today's incomplete candle if the REST API returned it (intraday boot).
	loc, _ := time.LoadLocation("Asia/Kolkata")
	if loc == nil {
		loc = time.FixedZone("IST", 5*3600+30*60)
	}
	now := time.Now().In(loc)
	lastDate := candles[len(candles)-1].Date.In(loc)
	if lastDate.Year() == now.Year() && lastDate.Month() == now.Month() && lastDate.Day() == now.Day() {
		candles = candles[:len(candles)-1]
	}

	_, marketOpen := ctx.Intraday.Get(tick.InstrumentToken)
	var currentPrice float64

	if marketOpen {
		currentPrice = tick.LastPrice
	} else {
		if len(candles) == 0 {
			return false, domain.OrderSignal{}
		}
		last := candles[len(candles)-1]
		currentPrice = last.Close
		// The last candle's close IS the current price, so it must not also be
		// counted as history.
		candles = candles[:len(candles)-1]
	}

	if len(candles) < h.MinCandles()-1 {
		return false, domain.OrderSignal{}
	}

	// Build the close series ending with the bar being evaluated.
	closes := make([]float64, 0, len(candles)+1)
	for _, c := range candles {
		closes = append(closes, c.Close)
	}
	closes = append(closes, currentPrice)

	rsi := rsiSeries(closes, h.RSIPeriod)
	ema := emaSeries(rsi, h.EMAPeriod)
	wma := wmaSeries(rsi, h.WMAPeriod)

	// Three WMA values: one for this bar's slope, one more for the previous
	// bar's. The other two lines only need two values each.
	if len(rsi) < 2 || len(ema) < 2 || len(wma) < 3 {
		return false, domain.OrderSignal{}
	}

	cur := h.classify(hmBar{
		RSI: at(rsi, 0), EMA: at(ema, 0), WMA: at(wma, 0), PrevWMA: at(wma, 1),
	})
	prev := h.classify(hmBar{
		RSI: at(rsi, 1), EMA: at(ema, 1), WMA: at(wma, 1), PrevWMA: at(wma, 2),
	})

	if !fired(cur, prev) {
		return false, domain.OrderSignal{}
	}
	if h.LongOnly && (cur == hmSell1 || cur == hmSell2) {
		return false, domain.OrderSignal{}
	}

	return true, domain.OrderSignal{
		InstrumentToken: tick.InstrumentToken,
		StrategyName:    h.Name() + " (" + cur.String() + ")",
		SignalTime:      time.Now().UTC(),
	}
}
