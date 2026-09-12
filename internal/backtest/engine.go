// Package backtest replays stored daily candle history through the live
// strategy.Strategy interface and measures forward returns at fixed holding
// horizons (1D / 5D / 1M / 3M by default).
//
// The replay exploits the closed-market code path that every strategy already
// implements: when MarketContext.Intraday holds no entry for a token, strategies
// treat the LAST candle in the slice as "today" and use its close as the current
// price. Feeding the engine a growing prefix of history therefore reproduces
// exactly what the strategy would have seen on the evening of that session —
// with no look-ahead, and with zero changes to the strategy code itself.
package backtest

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3" // SQLite driver registration via side-effect import
	kiteconnect "github.com/zerodha/gokiteconnect/v4"
	kitemodels "github.com/zerodha/gokiteconnect/v4/models"

	"algotrader/internal/domain"
	"algotrader/internal/strategy"
)

// tickModeFull mirrors kiteticker.ModeFull. It is duplicated here rather than
// imported so the backtester does not pull in the WebSocket client.
const tickModeFull = "full"

// Direction is the side a signal takes: long (profit when price rises) or
// short (profit when price falls). It determines the sign of a trade's return.
type Direction int8

const (
	// Long is a buy signal — return is positive when the exit price exceeds entry.
	Long Direction = iota
	// Short is a sell signal — return is positive when the exit price is below entry.
	Short
)

// String returns the human-readable side label used in reports.
func (d Direction) String() string {
	if d == Short {
		return "SHORT"
	}
	return "LONG"
}

// Horizon is a forward holding period measured in trading sessions (candles),
// not calendar days — 1M is 21 sessions, 3M is 63 sessions.
type Horizon struct {
	Label string
	Days  int
}

// DefaultHorizons are the report's standard holding periods: next session,
// one trading week, one trading month (~21 sessions), one quarter (~63 sessions).
var DefaultHorizons = []Horizon{
	{Label: "1D", Days: 1},
	{Label: "5D", Days: 5},
	{Label: "1M", Days: 21},
	{Label: "3M", Days: 63},
}

// Leg is the outcome of holding one trade for one horizon.
// Valid is false when the candle history ends before the horizon completes —
// such legs are excluded from every statistic rather than counted as flat.
type Leg struct {
	Valid     bool
	ExitDate  time.Time
	ExitPrice float64
	ReturnPct float64 // signed for Direction, net of Config.CostBps
	MaxFavPct float64 // best unrealised move reached before the exit (MFE)
	MaxAdvPct float64 // worst unrealised move reached before the exit (MAE)

	// Stopped is true when Config.StopSMA cut the trade short instead of it
	// running the full horizon. HeldDays is how many sessions it actually ran.
	Stopped  bool
	HeldDays int
}

// Trade is one signal fired by a strategy during the replay, together with its
// forward outcome at each configured horizon (Legs is index-aligned with Horizons).
type Trade struct {
	Strategy   string // strategy.Name(), e.g. "IchimokuKinkoHyo"
	Variant    string // full OrderSignal.StrategyName, e.g. "IchimokuKinkoHyo (BUY) Gold Cross"
	Token      uint32
	Symbol     string
	Direction  Direction
	EntryDate  time.Time
	EntryPrice float64
	Legs       []Leg

	// SessionIdx is the position of the signal session in this token's candle
	// series. It gives an exact session distance between two signals on the same
	// token, which calendar dates cannot (holidays, weekends).
	SessionIdx int

	// AlignGap is set only on confluence events: the number of sessions between
	// the first and the last agreeing signal. 0 means they fired the same day.
	AlignGap int
}

// Config parameterises a replay run.
type Config struct {
	// DBPath is the SQLite file holding the daily_candles table.
	DBPath string

	// Tokens is the instrument universe to replay. Tokens with fewer than
	// Warmup candles are skipped and counted in Result.Skipped.
	Tokens []uint32

	// Symbols maps instrument token to trading symbol for report labelling.
	// Tokens absent from the map fall back to a "T<token>" placeholder.
	Symbols map[uint32]string

	// Horizons are the forward holding periods to measure. Defaults to DefaultHorizons.
	Horizons []Horizon

	// Warmup is the minimum number of candles that must precede a session before
	// the strategy is evaluated on it. Ichimoku needs 105; RSI needs period+2.
	Warmup int

	// From and To bound the SIGNAL date (not the data load) — candles outside the
	// window are still fed to the strategy as history and used for exits. Zero
	// values mean unbounded.
	From, To time.Time

	// CostBps is the round-trip cost (brokerage + slippage + impact) in basis
	// points, subtracted from every leg's return. 20 bps ≈ 0.20%.
	CostBps float64

	// MaxMovePct discards any leg whose absolute move exceeds this percentage.
	// Unadjusted split and bonus records show up as a single -80% or +400% jump
	// that is not a tradeable outcome; without a cap one such row can dominate a
	// whole variant's average. 0 disables the filter.
	MaxMovePct float64

	// CooldownDays suppresses a repeat signal of the same variant on the same
	// token until this many sessions have passed. 0 disables the filter — every
	// firing is recorded, which is how the live engine behaves across days.
	CooldownDays int

	// StopSMA, if > 0, applies a moving-average stop-loss on a CLOSING basis: a
	// long is exited at the close of the first session that CLOSES below the
	// N-period simple moving average of closes, and a short at the first close
	// ABOVE it. The horizon then becomes a maximum holding period rather than a
	// fixed one. 0 disables the stop, leaving every trade to run its full term.
	//
	// Closing basis is the point — an intraday dip through the average does not
	// trigger, only a settled close beyond it, which is what makes the rule
	// tradeable from end-of-day data.
	StopSMA int

	// StopTrailLow turns StopSMA into a two-stage trailing stop. The close
	// through the average no longer exits: it ARMS a stop level at that
	// session's low (a short's at its high), effective the following session.
	// The trade runs until a later session breaks that level, and every further
	// close through the average re-arms it at the new extreme.
	//
	// The level only ever tightens. A stop allowed to retreat would let a losing
	// trade run further than the last level it already survived.
	StopTrailLow bool

	// StopIntraday triggers the armed level on the session's low (or high for a
	// short) touching it, filling at the level — how a resting stop order
	// behaves. A session that GAPS through fills at the open instead, since the
	// level was never available. When false only a CLOSE beyond the level exits,
	// and the fill is that close.
	StopIntraday bool

	// StopMaxLossPct is a hard floor under whatever StopSMA does: a resting order
	// this many percent adverse to entry, always filled intraday (gaps fill at
	// the open). It exists because a moving-average stop cannot arm until price
	// travels back through the average, which on a violent move is far too late.
	// 0 disables it. Works with or without StopSMA.
	StopMaxLossPct float64

	// Workers is the number of tokens replayed concurrently. Defaults to 8.
	Workers int

	// NewStrategy builds a FRESH strategy instance. It is called once per token
	// so that stateful strategies (RSI's range-shift machine) never leak state
	// from one instrument's replay into another's.
	NewStrategy func() strategy.Strategy

	// DirectionOf classifies a signal name as long or short. Defaults to
	// InferDirection when nil.
	DirectionOf func(signalName string) Direction

	// Progress, if set, is called as tokens complete. Safe to leave nil.
	Progress func(done, total int)
}

// Result is the full output of a replay: every trade plus run-level metadata.
type Result struct {
	StrategyName string
	Horizons     []Horizon
	Trades       []Trade
	Universe     int // tokens requested
	Replayed     int // tokens with enough history to evaluate
	Skipped      int // tokens skipped for insufficient history
	Sessions     int // total strategy evaluations performed
	Outliers     int // legs discarded by Config.MaxMovePct
	MaxMovePct   float64
	FirstSignal  time.Time // earliest signal date observed
	LastSignal   time.Time // latest signal date observed
	DataStart    time.Time // earliest candle date seen across the universe
	DataEnd      time.Time // latest candle date seen across the universe
	CostBps      float64
	StopSMA      int // 0 when trades ran their full horizon
	StopTrailLow bool
	StopIntraday bool
	StopMaxLoss  float64
	Elapsed      time.Duration

	// Notes are free-form lines rendered in the report header — used by the
	// confluence report to explain how its events were formed.
	Notes []string
}

// InferDirection classifies a signal name as Short when it carries bearish
// vocabulary ("SELL", "BEARISH", "DEATH", "SHORT"), and Long otherwise.
// Strategies in this repo encode the side in OrderSignal.StrategyName, e.g.
// "IchimokuKinkoHyo (SELL) Death Cross".
func InferDirection(signalName string) Direction {
	u := strings.ToUpper(signalName)
	for _, marker := range []string{"SELL", "BEARISH", "DEATH", "SHORT"} {
		if strings.Contains(u, marker) {
			return Short
		}
	}
	return Long
}

// Run replays the configured universe and returns every signal with its forward
// returns. Tokens are processed concurrently; the returned trades are sorted by
// entry date then symbol so reports are deterministic across runs.
func Run(cfg Config) (*Result, error) {
	if cfg.NewStrategy == nil {
		return nil, fmt.Errorf("backtest: Config.NewStrategy is required")
	}
	if len(cfg.Horizons) == 0 {
		cfg.Horizons = DefaultHorizons
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 8
	}
	if cfg.Warmup <= 0 {
		cfg.Warmup = 2
	}
	if cfg.DirectionOf == nil {
		cfg.DirectionOf = InferDirection
	}

	db, err := openReadOnly(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	started := time.Now()
	res := &Result{
		StrategyName: cfg.NewStrategy().Name(),
		Horizons:     cfg.Horizons,
		Universe:     len(cfg.Tokens),
		CostBps:      cfg.CostBps,
		StopSMA:      cfg.StopSMA,
		StopTrailLow: cfg.StopTrailLow,
		StopIntraday: cfg.StopIntraday,
		StopMaxLoss:  cfg.StopMaxLossPct,
		MaxMovePct:   cfg.MaxMovePct,
	}

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		done     int64
		sessions int64
		skipped  int64
		replayed int64
		outliers int64
	)

	tokens := make(chan uint32)
	for w := 0; w < cfg.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for token := range tokens {
				candles, err := loadCandles(db, token)
				n := atomic.AddInt64(&done, 1)
				if cfg.Progress != nil {
					cfg.Progress(int(n), len(cfg.Tokens))
				}
				if err != nil || len(candles) <= cfg.Warmup {
					atomic.AddInt64(&skipped, 1)
					continue
				}
				atomic.AddInt64(&replayed, 1)

				// A fresh instance per token so stateful strategies (RSI's
				// range-shift machine) start clean on every instrument.
				strat := cfg.NewStrategy()
				trades, evals, dropped := replayToken(cfg, strat, token, candles)
				atomic.AddInt64(&sessions, int64(evals))
				atomic.AddInt64(&outliers, int64(dropped))

				mu.Lock()
				res.Trades = append(res.Trades, trades...)
				trackSpan(&res.DataStart, &res.DataEnd, candles[0].Date.Time, candles[len(candles)-1].Date.Time)
				mu.Unlock()
			}
		}()
	}
	for _, t := range cfg.Tokens {
		tokens <- t
	}
	close(tokens)
	wg.Wait()

	res.Replayed = int(replayed)
	res.Skipped = int(skipped)
	res.Sessions = int(sessions)
	res.Outliers = int(outliers)
	res.Elapsed = time.Since(started)

	sort.SliceStable(res.Trades, func(i, j int) bool {
		if !res.Trades[i].EntryDate.Equal(res.Trades[j].EntryDate) {
			return res.Trades[i].EntryDate.Before(res.Trades[j].EntryDate)
		}
		if res.Trades[i].Symbol != res.Trades[j].Symbol {
			return res.Trades[i].Symbol < res.Trades[j].Symbol
		}
		return res.Trades[i].Variant < res.Trades[j].Variant
	})
	if len(res.Trades) > 0 {
		res.FirstSignal = res.Trades[0].EntryDate
		res.LastSignal = res.Trades[len(res.Trades)-1].EntryDate
	}
	return res, nil
}

// FilterTrades returns a copy of res keeping only the trades that satisfy keep.
//
// Filtering happens AFTER the replay, never during it: stateful strategies must
// still see every signal they would have fired live, or their internal state
// machines would diverge from reality. Only the recorded set narrows.
func FilterTrades(res *Result, note string, keep func(Trade) bool) *Result {
	out := *res
	out.Trades = nil
	for _, t := range res.Trades {
		if keep(t) {
			out.Trades = append(out.Trades, t)
		}
	}

	// Trades stay in the entry-date order Run established, so the ends are the span.
	out.FirstSignal, out.LastSignal = time.Time{}, time.Time{}
	if len(out.Trades) > 0 {
		out.FirstSignal = out.Trades[0].EntryDate
		out.LastSignal = out.Trades[len(out.Trades)-1].EntryDate
	}

	out.Notes = append(append([]string(nil), res.Notes...), note)
	return &out
}

// MatchesAny reports whether the variant contains any of the given fragments,
// compared case-insensitively. An empty fragment list matches everything.
func MatchesAny(variant string, fragments []string) bool {
	if len(fragments) == 0 {
		return true
	}
	v := strings.ToUpper(variant)
	for _, f := range fragments {
		if strings.Contains(v, strings.ToUpper(f)) {
			return true
		}
	}
	return false
}

// replayToken walks one instrument's history session by session, evaluating the
// strategy against the prefix of candles that would have been known at each
// close, and records a Trade for every signal fired.
//
// Returns the trades, the number of sessions evaluated, and the number of legs
// discarded as corporate-action outliers.
func replayToken(cfg Config, strat strategy.Strategy, token uint32, candles []kiteconnect.HistoricalData) ([]Trade, int, int) {
	symbol, ok := cfg.Symbols[token]
	if !ok || symbol == "" {
		symbol = fmt.Sprintf("T%d", token)
	}

	// A single MarketContext is reused for the whole token replay: Set() replaces
	// the slice wholesale, and Intraday stays empty so every strategy takes its
	// closed-market branch (last candle == "today").
	ctx := domain.NewMarketContext()

	// lastFire[variant] is the session index of that variant's previous signal,
	// used to enforce CooldownDays.
	lastFire := make(map[string]int)

	// The stop's moving average depends only on the token's closes, so it is
	// computed once here rather than per trade. A nil SMA disables the stop.
	stop := stopRule{
		TrailLow:   cfg.StopTrailLow,
		Intraday:   cfg.StopIntraday,
		MaxLossPct: cfg.StopMaxLossPct,
	}
	if cfg.StopSMA > 0 {
		stop.SMA = smaCloses(candles, cfg.StopSMA)
	}

	var trades []Trade
	evaluated, outliers := 0, 0

	for i := cfg.Warmup; i < len(candles); i++ {
		session := candles[i].Date.Time
		if !cfg.To.IsZero() && session.After(cfg.To) {
			break // history is ascending — nothing past this point is in window
		}

		// The strategy sees candles[0..i]; candles[i] is "today".
		ctx.Set(token, candles[:i+1])
		evaluated++

		// The synthetic tick mirrors the final print of the session. Strategies
		// only read InstrumentToken and LastPrice; OHLC is filled in so the tick
		// is internally consistent for any strategy added later.
		tick := kitemodels.Tick{
			Mode:            tickModeFull,
			InstrumentToken: token,
			IsTradable:      true,
			LastPrice:       candles[i].Close,
			Timestamp:       kitemodels.Time{Time: session},
			VolumeTraded:    uint32(candles[i].Volume),
			OHLC: kitemodels.OHLC{
				InstrumentToken: token,
				Open:            candles[i].Open,
				High:            candles[i].High,
				Low:             candles[i].Low,
				Close:           candles[i].Close,
			},
		}
		fired, sig := strat.Evaluate(tick, ctx)
		if !fired {
			continue
		}

		// The signal date filter is applied AFTER evaluation so that stateful
		// strategies still see the full run-up to the window.
		if !cfg.From.IsZero() && session.Before(cfg.From) {
			continue
		}
		variant := sig.StrategyName
		if variant == "" {
			variant = strat.Name()
		}
		if cfg.CooldownDays > 0 {
			if prev, seen := lastFire[variant]; seen && i-prev < cfg.CooldownDays {
				continue
			}
		}
		lastFire[variant] = i

		dir := cfg.DirectionOf(variant)
		entry := candles[i].Close
		trade := Trade{
			Strategy:   strat.Name(),
			Variant:    variant,
			Token:      token,
			Symbol:     symbol,
			Direction:  dir,
			EntryDate:  session,
			EntryPrice: entry,
			SessionIdx: i,
			Legs:       make([]Leg, len(cfg.Horizons)),
		}
		for h, hz := range cfg.Horizons {
			leg := measureLeg(candles, i, hz.Days, entry, dir, cfg.CostBps, stop)
			// A move this large on a daily close is almost always an unadjusted
			// split or bonus, not a return anyone could have earned.
			if leg.Valid && cfg.MaxMovePct > 0 && math.Abs(leg.ReturnPct) > cfg.MaxMovePct {
				leg = Leg{Valid: false}
				outliers++
			}
			trade.Legs[h] = leg
		}
		trades = append(trades, trade)
	}
	return trades, evaluated, outliers
}

// smaCloses returns the `period`-bar simple moving average of closes at every
// index, with NaN wherever there is not yet enough history. It is computed once
// per token and shared by every leg of every trade on that token.
func smaCloses(candles []kiteconnect.HistoricalData, period int) []float64 {
	if period <= 0 {
		return nil
	}
	out := make([]float64, len(candles))
	var sum float64
	for i, c := range candles {
		sum += c.Close
		if i >= period {
			sum -= candles[i-period].Close
		}
		if i < period-1 {
			out[i] = math.NaN()
			continue
		}
		out[i] = sum / float64(period)
	}
	return out
}

// stopRule describes how a leg may be closed before its horizon expires.
type stopRule struct {
	SMA      []float64 // nil disables the moving-average stop
	TrailLow bool
	Intraday bool

	// MaxLossPct is a hard floor underneath whatever the SMA rule does, as a
	// percentage adverse move from entry. 0 disables it.
	MaxLossPct float64
}

func (s stopRule) active() bool { return s.SMA != nil || s.MaxLossPct > 0 }

// hardLevel is the fixed price the max-loss stop rests at, or NaN when none is
// configured.
func (s stopRule) hardLevel(entry float64, dir Direction) float64 {
	if s.MaxLossPct <= 0 {
		return math.NaN()
	}
	if dir == Long {
		return entry * (1 - s.MaxLossPct/100)
	}
	return entry * (1 + s.MaxLossPct/100)
}

// firstTouched returns whichever of two resting levels a session reaches first
// as price moves against the position — the higher for a long, the lower for a
// short. A NaN operand means that level is not set.
func firstTouched(a, b float64, dir Direction) float64 {
	switch {
	case math.IsNaN(a):
		return b
	case math.IsNaN(b):
		return a
	case dir == Long:
		return math.Max(a, b)
	}
	return math.Min(a, b)
}

// breached reports whether session c takes out the stop level, and the price the
// exit fills at. A session that gaps straight through fills at the open, because
// the level itself was never traded.
func breached(c kiteconnect.HistoricalData, level float64, dir Direction, intraday bool) (bool, float64) {
	if dir == Long {
		if intraday {
			if c.Low <= level {
				return true, math.Min(level, c.Open)
			}
		} else if c.Close < level {
			return true, c.Close
		}
		return false, 0
	}
	if intraday {
		if c.High >= level {
			return true, math.Max(level, c.Open)
		}
	} else if c.Close > level {
		return true, c.Close
	}
	return false, 0
}

// runStop walks the holding window and returns the session the trade was closed
// on, the price it filled at, and whether any stop bound at all.
//
// Within one session the order of events is fixed: resting orders (the hard
// max-loss floor, and the trailing level when it is intraday-triggered) can only
// fill during the session, so they are tested before any close-based rule. When
// two resting levels are live, price reaches the nearer one first.
//
// The trailing level is tested BEFORE this session can arm or tighten it, which
// is what makes it effective "the next day" — a level set at today's low can
// never be triggered by today's own low.
func runStop(candles []kiteconnect.HistoricalData, entryIdx, horizonIdx int, entry float64, dir Direction, stop stopRule) (int, float64, bool) {
	hard := stop.hardLevel(entry, dir)
	level := math.NaN() // trailing level; nothing armed until the average breaks

	for k := entryIdx + 1; k <= horizonIdx; k++ {
		// ── Resting orders, filled intraday ──────────────────────────────────
		resting := hard
		if stop.TrailLow && stop.Intraday {
			resting = firstTouched(resting, level, dir)
		}
		if !math.IsNaN(resting) {
			if hit, fill := breached(candles[k], resting, dir, true); hit {
				return k, fill, true
			}
		}

		if stop.SMA == nil {
			continue
		}
		avg := stop.SMA[k]
		if math.IsNaN(avg) {
			continue // average not yet defined this early in the series
		}

		// ── Close-based rules ────────────────────────────────────────────────
		if !stop.TrailLow {
			// Immediate mode: settle on the losing side of the average and exit.
			if (dir == Long && candles[k].Close < avg) || (dir == Short && candles[k].Close > avg) {
				return k, candles[k].Close, true
			}
			continue
		}

		if !stop.Intraday && !math.IsNaN(level) {
			if hit, fill := breached(candles[k], level, dir, false); hit {
				return k, fill, true
			}
		}

		// Arm or tighten. Never loosen — see Config.StopTrailLow.
		if dir == Long && candles[k].Close < avg {
			if math.IsNaN(level) || candles[k].Low > level {
				level = candles[k].Low
			}
		} else if dir == Short && candles[k].Close > avg {
			if math.IsNaN(level) || candles[k].High < level {
				level = candles[k].High
			}
		}
	}
	return horizonIdx, 0, false
}

// measureLeg computes the outcome of entering at the close of candles[entryIdx]
// and exiting at the close `days` sessions later, along with the best (MFE) and
// worst (MAE) unrealised excursions reached in between.
//
// Entry is at the signal session's close — the earliest price actually
// obtainable once an end-of-day signal exists, so the fill is never optimistic.
//
// When a stop is configured the horizon becomes a MAXIMUM holding period. The
// check starts the session AFTER entry, so a trade can never be stopped out on
// the bar that opened it.
//
// A stopped trade counts even when the full horizon runs past the end of the
// data: its outcome is known the moment the stop fires. Requiring the whole
// horizon to fit would silently discard every signal within `days` sessions of
// today — with a long horizon and a stop that exits in ~20 sessions, that is
// most of the recent sample. A trade still OPEN at the data edge is excluded,
// since its outcome genuinely is not known yet.
func measureLeg(candles []kiteconnect.HistoricalData, entryIdx, days int, entry float64, dir Direction, costBps float64, stop stopRule) Leg {
	horizonIdx := entryIdx + days
	lastIdx := len(candles) - 1

	searchTo, truncated := horizonIdx, false
	if searchTo > lastIdx {
		if !stop.active() {
			return Leg{Valid: false} // no stop, so the horizon is the only exit
		}
		searchTo, truncated = lastIdx, true
	}
	if entry <= 0 || searchTo <= entryIdx {
		return Leg{Valid: false}
	}

	stopped := false
	exit := 0.0
	exitIdx := searchTo
	if stop.active() {
		exitIdx, exit, stopped = runStop(candles, entryIdx, searchTo, entry, dir, stop)
	}
	if !stopped {
		if truncated {
			return Leg{Valid: false} // still open where the data ends
		}
		exitIdx = horizonIdx
		exit = candles[exitIdx].Close
	}

	leg := Leg{
		Valid:     true,
		ExitDate:  candles[exitIdx].Date.Time,
		ExitPrice: exit,
		ReturnPct: signedReturn(entry, exit, dir) - costBps/100.0,
		Stopped:   stopped,
		HeldDays:  exitIdx - entryIdx,
	}

	// Excursions are measured against the intra-window highs and lows, up to
	// whichever exit actually occurred.
	best, worst := leg.ReturnPct, leg.ReturnPct
	for k := entryIdx + 1; k <= exitIdx; k++ {
		up := signedReturn(entry, candles[k].High, dir)
		down := signedReturn(entry, candles[k].Low, dir)
		if dir == Short {
			up, down = down, up // a low is favourable when short
		}
		if up > best {
			best = up
		}
		if down < worst {
			worst = down
		}
	}
	leg.MaxFavPct = best
	leg.MaxAdvPct = worst
	return leg
}

// signedReturn expresses the move from entry to exit as a percentage that is
// positive whenever the trade made money on the given side.
func signedReturn(entry, exit float64, dir Direction) float64 {
	if entry <= 0 {
		return 0
	}
	r := (exit - entry) / entry * 100.0
	if dir == Short {
		return -r
	}
	return r
}

// trackSpan widens [start, end] to cover the given candle range.
func trackSpan(start, end *time.Time, first, last time.Time) {
	if start.IsZero() || first.Before(*start) {
		*start = first
	}
	if end.IsZero() || last.After(*end) {
		*end = last
	}
}

// openReadOnly opens the candle database read-only so a replay can never mutate
// production data and can run while the live engine holds the file.
func openReadOnly(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?mode=ro&_journal_mode=WAL&_cache_size=20000", path))
	if err != nil {
		return nil, fmt.Errorf("backtest: open %q: %w", path, err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("backtest: ping %q: %w", path, err)
	}
	return db, nil
}

// LoadUniverse returns every distinct instrument token present in daily_candles.
// Used when no Kite credentials are available to narrow the universe by index.
func LoadUniverse(dbPath string) ([]uint32, error) {
	db, err := openReadOnly(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`SELECT DISTINCT token FROM daily_candles ORDER BY token`)
	if err != nil {
		return nil, fmt.Errorf("backtest: LoadUniverse query: %w", err)
	}
	defer rows.Close()

	var tokens []uint32
	for rows.Next() {
		var t uint32
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("backtest: LoadUniverse scan: %w", err)
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// loadCandles reads a token's full daily history in ascending date order.
// Unlike repository.LoadHistory it applies no 3-year cutoff — a backtest wants
// every session the database holds.
func loadCandles(db *sql.DB, token uint32) ([]kiteconnect.HistoricalData, error) {
	rows, err := db.Query(
		`SELECT date, open, high, low, close, volume
		 FROM daily_candles WHERE token = ? ORDER BY date ASC`, token)
	if err != nil {
		return nil, fmt.Errorf("backtest: loadCandles token %d: %w", token, err)
	}
	defer rows.Close()

	candles := make([]kiteconnect.HistoricalData, 0, 1024)
	for rows.Next() {
		var (
			dateStr                 string
			open, high, low, closep float64
			volume                  int64
		)
		if err := rows.Scan(&dateStr, &open, &high, &low, &closep, &volume); err != nil {
			return nil, fmt.Errorf("backtest: loadCandles scan token %d: %w", token, err)
		}
		t, err := time.Parse(time.RFC3339, dateStr)
		if err != nil {
			t, err = time.Parse("2006-01-02", dateStr)
			if err != nil {
				continue // unparseable row — skip rather than abort the whole token
			}
		}
		candles = append(candles, kiteconnect.HistoricalData{
			Date:   kitemodels.Time{Time: t},
			Open:   open,
			High:   high,
			Low:    low,
			Close:  closep,
			Volume: int(volume),
		})
	}
	return candles, rows.Err()
}
