package strategy

import (
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
	"github.com/zerodha/gokiteconnect/v4/models"

	"algotrader/internal/domain"
)

// RSI emits exactly two signals, both GFS (Grandfather–Father–Son) setups.
//
// Every GFS setup asks the same question on the two higher timeframes — is the
// stock in an established uptrend? — and differs only in where the daily leg
// turns up:
//
//	RSI (GFS)          monthly RSI > 60, weekly RSI > 60, daily turns up at 40
//	RSI (Advanced GFS) monthly RSI > 60, weekly RSI > 60, daily turns up at 60
//
// GFS buys the deep pullback: the daily has given back most of its momentum and
// is turning at the bear/bull boundary. Advanced GFS buys the shallow pause: the
// daily never left bullish territory at all.
//
// The strategy is stateless — every decision reads the candle series and the two
// most recent RSI values, nothing else. One instance is safe to share across all
// instruments and all worker goroutines.
type RSI struct {
	Period int

	// WeeklyPeriod and MonthlyPeriod are the RSI lookbacks applied to the
	// resampled higher-timeframe series. They default to Period.
	WeeklyPeriod  int
	MonthlyPeriod int

	// MTFLevel is the RSI floor both higher timeframes must clear. 60 marks the
	// boundary of strong bullish territory.
	MTFLevel float64
}

func NewRSI(period int) *RSI {
	if period <= 0 {
		period = 14 // default Wilder's smoothing period
	}
	return &RSI{
		Period:        period,
		WeeklyPeriod:  period,
		MonthlyPeriod: period,
		MTFLevel:      60,
	}
}

func (r *RSI) Name() string { return "RSI" }

// turnUpBand describes a daily leg: the band yesterday's RSI must sit in, and
// the level today's RSI must clear on the way back up.
type turnUpBand struct {
	Lower float64
	Upper float64
	Level float64
}

var (
	// gfsBand is the daily leg of GFS: 40 as the pivot.
	gfsBand = turnUpBand{Lower: 37, Upper: 44, Level: 40}

	// advancedGFSBand is the daily leg of Advanced GFS: 60 as the pivot.
	advancedGFSBand = turnUpBand{Lower: 57, Upper: 63, Level: 60}
)

// turnedUpAt reports whether the daily RSI turned up at this band's level on the
// bar being evaluated:
//
//	yesterday sat inside the band straddling the level — at it, neither
//	extended above nor broken below;
//	today closed higher than yesterday — the turn is up, not down;
//	today is above the level — the turn happened on the bullish side of it.
//
// The test is memoryless: it reads two consecutive bars and nothing else, so it
// needs no prior cross or momentum leg, and it can repeat on consecutive
// sessions while RSI grinds up through the band. De-duplication belongs to the
// caller (StateRegistry live, -cooldown in the backtester).
//
// An unknown prevRSI is 0, which fails the lower bound, so no extra guard is
// needed for the warm-up window.
func turnedUpAt(prevRSI, rsi float64, b turnUpBand) bool {
	return prevRSI >= b.Lower && prevRSI <= b.Upper && rsi > prevRSI && rsi > b.Level
}

func calculateRSI(candles []kiteconnect.HistoricalData, period int, currentPrice float64) float64 {
	if len(candles) < period {
		return 0
	}

	prices := make([]float64, 0, len(candles)+1)
	for _, c := range candles {
		prices = append(prices, c.Close)
	}
	prices = append(prices, currentPrice)

	return rsiFromCloses(prices, period)
}

// rsiFromCloses computes Wilder's RSI over a closing-price series that already
// ends with the current price. It returns 0 when the series is too short to
// seed the average, which callers treat as "unknown" rather than "oversold".
func rsiFromCloses(prices []float64, period int) float64 {
	if period <= 0 || len(prices) <= period {
		return 0
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
	}

	return rsiFrom(avgGain, avgLoss)
}

// dailyClose is one session's closing price with the date it belongs to.
// It is the input to weekly and monthly resampling.
type dailyClose struct {
	Date  time.Time
	Close float64
}

// resampleCloses collapses a daily close series into higher-timeframe closes by
// keeping the LAST close of each bucket, which is what a weekly or monthly
// candle closes at. The series must be in ascending date order.
//
// The final bucket is normally still in progress (mid-week or mid-month); its
// running close is included deliberately, matching how charting platforms show
// a live higher-timeframe bar.
func resampleCloses(series []dailyClose, bucketOf func(time.Time) (int, int)) []float64 {
	if len(series) == 0 {
		return nil
	}

	closes := make([]float64, 0, len(series)/5+1)
	curA, curB := bucketOf(series[0].Date)
	last := series[0].Close

	for _, bar := range series[1:] {
		a, b := bucketOf(bar.Date)
		if a != curA || b != curB {
			closes = append(closes, last) // previous bucket just completed
			curA, curB = a, b
		}
		last = bar.Close
	}
	return append(closes, last) // the in-progress bucket
}

// weekBucket keys a date by ISO year and ISO week, so weeks spanning a year
// boundary stay a single bucket.
func weekBucket(t time.Time) (int, int) { return t.ISOWeek() }

// monthBucket keys a date by calendar year and month.
func monthBucket(t time.Time) (int, int) { return t.Year(), int(t.Month()) }

// higherTimeframesBullish reports whether BOTH the weekly and the monthly RSI
// sit above MTFLevel, together with the two values for logging. These are the
// Grandfather and Father legs, shared by both GFS variants.
//
// A zero RSI means the series was too short to compute (a monthly RSI(14) needs
// roughly 15 months of daily history). That is reported as not-bullish: an
// unknown higher timeframe must never be read as confirmation.
func (r *RSI) higherTimeframesBullish(series []dailyClose) (weekly, monthly float64, ok bool) {
	weekly = rsiFromCloses(resampleCloses(series, weekBucket), r.WeeklyPeriod)
	monthly = rsiFromCloses(resampleCloses(series, monthBucket), r.MonthlyPeriod)
	return weekly, monthly, weekly > r.MTFLevel && monthly > r.MTFLevel
}

func (r *RSI) Evaluate(tick models.Tick, ctx *domain.MarketContext) (bool, domain.OrderSignal) {
	rawCandles, ok := ctx.Get(tick.InstrumentToken)
	if !ok || len(rawCandles) < r.Period {
		return false, domain.OrderSignal{}
	}

	// Copy the slice so we don't modify the shared array backing
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
	var currentDate time.Time

	if marketOpen {
		currentPrice = tick.LastPrice
		currentDate = now
	} else {
		// Market is closed: use last candle close
		if len(candles) == 0 {
			return false, domain.OrderSignal{}
		}
		last := candles[len(candles)-1]
		currentPrice = last.Close
		currentDate = last.Date.Time
		// We remove the last candle since we're using its close as the "current price"
		candles = candles[:len(candles)-1]
	}

	if len(candles) < r.Period {
		return false, domain.OrderSignal{}
	}

	// ── Current RSI ───────────────────────────────────────────────────────────
	rsiValue := calculateRSI(candles, r.Period, currentPrice)

	// ── Previous RSI (one candle back) ────────────────────────────────────────
	// Use the last candle's close as the "previous current price" and the candles
	// before it as the historical window. This gives us the RSI as it stood one
	// tick/candle ago, which is what every daily leg compares against.
	var prevRSI float64
	if len(candles) >= r.Period+1 {
		prevCurrentPrice := candles[len(candles)-1].Close
		prevCandles := candles[:len(candles)-1]
		prevRSI = calculateRSI(prevCandles, r.Period, prevCurrentPrice)
	}

	// ── Son: the daily leg ────────────────────────────────────────────────────
	// Checked first because it is two float comparisons, where the higher
	// timeframes cost two resamples and two more RSI passes. The vast majority of
	// ticks fail here and never pay for that.
	//
	// The two bands are disjoint (37–44 against 57–63), so at most one can match.
	advanced := turnedUpAt(prevRSI, rsiValue, advancedGFSBand)
	classic := turnedUpAt(prevRSI, rsiValue, gfsBand)
	if !advanced && !classic {
		return false, domain.OrderSignal{}
	}

	// ── Grandfather and Father: the higher timeframes ─────────────────────────
	// mtfSeries is the daily close series INCLUDING the bar being evaluated —
	// `candles` has had that bar removed, so it is re-attached here with
	// currentPrice as its close. Weekly and monthly RSI are resampled from this.
	mtfSeries := make([]dailyClose, 0, len(candles)+1)
	for _, c := range candles {
		mtfSeries = append(mtfSeries, dailyClose{Date: c.Date.Time, Close: c.Close})
	}
	mtfSeries = append(mtfSeries, dailyClose{Date: currentDate, Close: currentPrice})

	if _, _, confirmed := r.higherTimeframesBullish(mtfSeries); !confirmed {
		return false, domain.OrderSignal{}
	}

	name := r.Name() + " (GFS)"
	if advanced {
		name = r.Name() + " (Advanced GFS)"
	}

	return true, domain.OrderSignal{
		InstrumentToken: tick.InstrumentToken,
		TradingSymbol:   "",
		StrategyName:    name,
		SignalTime:      time.Now().UTC(),
	}
}
