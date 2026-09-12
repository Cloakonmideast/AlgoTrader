package strategy

import (
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
	"github.com/zerodha/gokiteconnect/v4/models"

	"algotrader/internal/domain"
)

type IchimokuKinkoHyo struct{}

func NewIchimokuKinkoHyo() *IchimokuKinkoHyo { return &IchimokuKinkoHyo{} }

func (i *IchimokuKinkoHyo) Name() string { return "IchimokuKinkoHyo" }

// periodMidpoint returns (highest high + lowest low) / 2 over a candle slice,
// seeded with today's live intraday high/low so the current session is included.
func periodMidpoint(candles []kiteconnect.HistoricalData, seedHigh, seedLow float64) float64 {
	high, low := seedHigh, seedLow
	for _, c := range candles {
		if c.High > high {
			high = c.High
		}
		if c.Low < low {
			low = c.Low
		}
	}
	return (high + low) / 2.0
}

func (i *IchimokuKinkoHyo) Evaluate(tick models.Tick, ctx *domain.MarketContext) (bool, domain.OrderSignal) {

	// ── 1. Data retrieval ─────────────────────────────────────────────────────

	rawCandles, ok := ctx.Get(tick.InstrumentToken)
	if !ok || len(rawCandles) < 105 {
		return false, domain.OrderSignal{}
	}

	// Copy the slice so we don't modify the shared array backing
	candles := make([]kiteconnect.HistoricalData, len(rawCandles))
	copy(candles, rawCandles)

	// Strip today's incomplete candle if the REST API returned it (intraday boot).
	// This ensures `candles` always ends with yesterday's completed daily candle.
	var trueTodayOpen float64
	loc, _ := time.LoadLocation("Asia/Kolkata")
	if loc == nil {
		loc = time.FixedZone("IST", 5*3600+30*60)
	}
	now := time.Now().In(loc)
	lastDate := candles[len(candles)-1].Date.In(loc)
	if lastDate.Year() == now.Year() && lastDate.Month() == now.Month() && lastDate.Day() == now.Day() {
		trueTodayOpen = candles[len(candles)-1].Open
		candles = candles[:len(candles)-1]
	}

	// 105 minimum: 104 for Leading Span B + 1 extra for cross detection
	if len(candles) < 105 {
		return false, domain.OrderSignal{}
	}

	// ── Live vs. closed-market detection ─────────────────────────────────────
	// ctx.Intraday.Get returns false when no tick has been received today,
	// which means the market is closed (holiday / weekend / pre-open).
	// In that case we use the last closed candle as the "today" seed so that
	// all period counts remain correct without corrupting the midpoint.
	intraday, marketOpen := ctx.Intraday.Get(tick.InstrumentToken)

	var todayHigh, todayLow, todayOpen float64
	var conversionCandles, baseCandles []kiteconnect.HistoricalData
	var yesterday kiteconnect.HistoricalData
	var cnt int

	var currentPrice float64
	var yesterdayClose float64

	if marketOpen {
		// Market is running: use live tick price and live intraday H/L seed.
		// candles[len-1] = yesterday's completed daily candle → correct "yesterday".
		currentPrice = tick.LastPrice
		todayHigh = intraday.High
		todayLow = intraday.Low
		yesterday = candles[len(candles)-1]
		if trueTodayOpen > 0 {
			todayOpen = trueTodayOpen
		} else {
			todayOpen = intraday.Open
		}
		yesterdayClose = candles[len(candles)-1].Close // last closed day = yesterday ✓
		conversionCandles = candles[len(candles)-8:]
		baseCandles = candles[len(candles)-25:]
		cnt = 25
	} else {
		// Market is closed: treat candles[len-1] as "today" (the most recent
		// session) so all Ichimoku period counts stay correct.
		// Therefore "yesterday" = candles[len-2].
		last := candles[len(candles)-1]
		currentPrice = last.Close
		todayHigh = last.High
		todayLow = last.Low
		todayOpen = last.Open
		yesterday = candles[len(candles)-2]
		yesterdayClose = candles[len(candles)-2].Close // one session earlier ✓
		conversionCandles = candles[len(candles)-9:]
		baseCandles = candles[len(candles)-26:]
		cnt = 26
	}

	// ── 2. Conversion Line (Tenkan-sen) — 9-period midpoint ──────────────────
	// "The average of the 9-period high and the 9-period low"
	conversionLine := periodMidpoint(conversionCandles, todayHigh, todayLow)

	// Previous Conversion Line (one session back) — needed for crossover detection
	//yesterday := candles[len(candles)-1]
	prevConversionLine := periodMidpoint(candles[len(candles)-9:len(candles)-1], yesterday.High, yesterday.Low)

	// ── 3. Base Line (Kijun-sen) — 26-period midpoint ────────────────────────
	// "The average of the 26-period high and the 26-period low"
	baseLine := periodMidpoint(baseCandles, todayHigh, todayLow)

	prevBaseLine := periodMidpoint(candles[len(candles)-26:len(candles)-1], yesterday.High, yesterday.Low)

	// ── 4. Leading Span A (Senkou Span A) ────────────────────────────────────
	// "The average of the Conversion Line and the Base Line, plotted 26 periods ahead"
	// To get the cloud visible TODAY, we use the Conversion Line and Base Line
	// that were calculated 26 sessions ago, seeded by that session's candle.
	sessionMinus26 := candles[len(candles)-cnt] // the "today" of 26 sessions ago
	sessionMinus27 := candles[len(candles)-(cnt+1)]
	// pastConversionLine: 8 closed candles before sessionMinus26 + sessionMinus26 seed = 9 periods
	pastConversionLine := periodMidpoint(candles[len(candles)-(cnt+8):len(candles)-cnt], sessionMinus26.High, sessionMinus26.Low)
	pastPrevConversionLine := periodMidpoint(candles[len(candles)-(cnt+9):len(candles)-(cnt+1)], sessionMinus27.High, sessionMinus27.Low)
	// pastBaseLine: 25 closed candles before sessionMinus26 + sessionMinus26 seed = 26 periods
	pastBaseLine := periodMidpoint(candles[len(candles)-(cnt+25):len(candles)-cnt], sessionMinus26.High, sessionMinus26.Low)
	leadingSpanA := (pastConversionLine + pastBaseLine) / 2.0

	sessionMinus52 := candles[len(candles)-(cnt+26)] // the "today" of 52 sessions ago
	pastConversionLine52 := periodMidpoint(candles[len(candles)-(cnt+26+8):len(candles)-(cnt+26)], sessionMinus52.High, sessionMinus52.Low)
	pastBaseLine52 := periodMidpoint(candles[len(candles)-(cnt+26+25):len(candles)-(cnt+26)], sessionMinus52.High, sessionMinus52.Low)
	leadingSpanA52 := (pastConversionLine52 + pastBaseLine52) / 2.0

	// ── 5. Leading Span B (Senkou Span B) ────────────────────────────────────
	// "The average of the 52-period high and the 52-period low, plotted 26 periods ahead"
	// Same shift logic as Leading Span A — use the window ending 26 sessions ago.
	// NOTE: strict calculation needs 78 candles (52 + 26 shift). With only 53
	// leadingSpanB: 51 closed candles before sessionMinus26 + sessionMinus26 seed = 52 periods
	leadingSpanB := periodMidpoint(candles[len(candles)-(cnt+51):len(candles)-cnt], sessionMinus26.High, sessionMinus26.Low)
	leadingSpanB52 := periodMidpoint(candles[len(candles)-(cnt+26+51):len(candles)-(cnt+26)], sessionMinus52.High, sessionMinus52.Low)
	// ── 6. Lagging Span (Chikou Span) ─────────────────────────────────────────
	// "The current closing price plotted 26 periods in the past"
	// Bullish when current price is above the high from 26 sessions ago
	// (i.e. the Lagging Span sits above historical price action)
	priceFrom26SessionsAgo := candles[len(candles)-(cnt+1)].High
	priceFrom27SessionsAgo := candles[len(candles)-(cnt+2)].High
	priceFrom26SessionsAgolow := candles[len(candles)-(cnt+1)].Low
	priceFrom27SessionsAgolow := candles[len(candles)-(cnt+2)].Low

	// ── 7. Cloud (Kumo) boundaries ───────────────────────────────────────────
	cloudTop := leadingSpanA
	cloudBottom := leadingSpanB
	if leadingSpanB > leadingSpanA {
		cloudTop = leadingSpanB
		cloudBottom = leadingSpanA
	}

	cloudTop52 := leadingSpanA52
	cloudBottom52 := leadingSpanB52
	if leadingSpanB52 > leadingSpanA52 {
		cloudTop52 = leadingSpanB52
		cloudBottom52 = leadingSpanA52
	}

	// ── 8. Signal conditions ──────────────────────────────────────────────────

	// Condition 1 — Bullish Gold Cross (Conversion Line crosses above Base Line)
	// "The most important short-term signal in Ichimoku"
	//tkCross := conversionLine > baseLine

	// Condition 2 — Price is above the Cloud
	// "Confirms the trend direction; above = bullish"
	//priceAboveCloud := currentPrice > cloudTop

	// Condition 3 — Lagging Span confirmation
	// "Current price (acting as Lagging Span) is above price from 26 sessions ago"
	//laggingSpanBullish := currentPrice > priceFrom26SessionsAgo && currentPrice > cloudTop52

	// Condition 4 — Bullish Cloud (Leading Span A above Leading Span B)
	// "Green cloud = bullish momentum; red cloud = bearish"
	//bullishCloud := leadingSpanA > leadingSpanB

	curr_above_cloud := currentPrice > cloudTop
	lag_above_cdlhigh := currentPrice > priceFrom26SessionsAgo
	cl_above_bl := conversionLine >= baseLine
	curr_above_cl := currentPrice > conversionLine
	lag_above_prev_ctop := currentPrice > cloudTop52
	condition12 := yesterdayClose < priceFrom27SessionsAgo

	// Condition 6 — Cloud cross-out filter (the new addition).
	// Fires only when price has broken ABOVE the cloud for the first time TODAY.
	// Two scenarios, either of which qualifies:
	//
	//   A) Intraday breakout: today's open was at or below the cloud top
	//      (price started the session inside or below the cloud), and the
	//      current tick is now above the cloud. The stock crossed out during
	//      the live session.
	//
	//   B) Gap-open breakout: yesterday's close was inside or below the cloud
	//      (price ended the previous session not yet above the cloud), and
	//      today's open gapped directly above it. The cross happened overnight.
	//
	// Without this filter, condition1 (currentPrice > cloudTop) would fire on
	// every subsequent tick for a stock that has been above the cloud for weeks.
	// condition6 ensures the alert is a fresh event, not a stale one.
	intradayBreakout := todayOpen <= cloudTop && currentPrice > cloudTop
	gapOpenBreakout := yesterdayClose <= cloudTop && todayOpen > cloudTop
	condition6 := intradayBreakout || gapOpenBreakout

	intradaySell := todayOpen >= cloudBottom && currentPrice < cloudBottom
	gapOpenSell := yesterdayClose >= cloudBottom && todayOpen < cloudBottom
	condition7 := intradaySell || gapOpenSell
	bl_above_cl := baseLine > conversionLine
	cl_above_curr := conversionLine > currentPrice
	lag_below_prev_cld_btm := cloudBottom52 > currentPrice
	lag_below_cdl := currentPrice < priceFrom26SessionsAgolow
	condition13 := yesterdayClose > priceFrom27SessionsAgolow
	curr_below_cld_btm := currentPrice < cloudBottom
	gold_cross := prevBaseLine >= prevConversionLine && baseLine < conversionLine
	lag_above_prevcl := currentPrice > pastConversionLine
	lag_cross := yesterdayClose < pastPrevConversionLine && currentPrice > pastConversionLine
	death_cross := prevConversionLine >= prevBaseLine && baseLine > conversionLine
	cl_above_cdl := conversionLine > currentPrice
	lag_below_cl := pastConversionLine > currentPrice
	lag_death_cross := yesterdayClose >= pastPrevConversionLine && currentPrice < pastConversionLine

	// ── 9. Full Ichimoku buy signal ───────────────────────────────────────────
	if curr_above_cloud && lag_above_cdlhigh && cl_above_bl && curr_above_cl && lag_above_prev_ctop && condition6 {
		return true, domain.OrderSignal{
			InstrumentToken: tick.InstrumentToken,
			TradingSymbol:   "",
			StrategyName:    i.Name() + " (BUY)",
			SignalTime:      time.Now().UTC(),
		}
	}
	if curr_above_cloud && lag_above_cdlhigh && cl_above_bl && curr_above_cl && lag_above_prev_ctop && condition12 {
		return true, domain.OrderSignal{
			InstrumentToken: tick.InstrumentToken,
			TradingSymbol:   "",
			StrategyName:    i.Name() + " (BUY) 1A",
			SignalTime:      time.Now().UTC(),
		}

	}

	if gold_cross && lag_above_cdlhigh && lag_above_prevcl && curr_above_cl {

		return true, domain.OrderSignal{
			InstrumentToken: tick.InstrumentToken,
			TradingSymbol:   "",
			StrategyName:    i.Name() + " (BUY) Gold Cross",
			SignalTime:      time.Now().UTC(),
		}
	}

	if cl_above_bl && lag_above_cdlhigh && curr_above_cl && lag_cross {
		return true, domain.OrderSignal{
			InstrumentToken: tick.InstrumentToken,
			TradingSymbol:   "",
			StrategyName:    i.Name() + " (BUY) Gold Cross A",
			SignalTime:      time.Now().UTC(),
		}
	}

	if condition7 && bl_above_cl && cl_above_curr && lag_below_prev_cld_btm && lag_below_cdl && curr_below_cld_btm {
		return true, domain.OrderSignal{
			InstrumentToken: tick.InstrumentToken,
			TradingSymbol:   "",
			StrategyName:    i.Name() + " (SELL)",
			SignalTime:      time.Now().UTC(),
		}

	}

	if bl_above_cl && cl_above_curr && lag_below_prev_cld_btm && lag_below_cdl && curr_below_cld_btm && condition13 {
		return true, domain.OrderSignal{
			InstrumentToken: tick.InstrumentToken,
			TradingSymbol:   "",
			StrategyName:    i.Name() + " (SELL) 1A",
			SignalTime:      time.Now().UTC(),
		}
	}

	if death_cross && cl_above_cdl && lag_below_cdl && lag_below_cl {
		return true, domain.OrderSignal{
			InstrumentToken: tick.InstrumentToken,
			TradingSymbol:   "",
			StrategyName:    i.Name() + " (SELL) Death Cross",
			SignalTime:      time.Now().UTC(),
		}
	}

	if lag_death_cross && cl_above_cdl && bl_above_cl {
		return true, domain.OrderSignal{
			InstrumentToken: tick.InstrumentToken,
			TradingSymbol:   "",
			StrategyName:    i.Name() + " (SELL) Death Cross A",
			SignalTime:      time.Now().UTC(),
		}
	}

	return false, domain.OrderSignal{}
}
