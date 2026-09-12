package backtest

import (
	"math"
	"sort"
	"strconv"
)

// Stats summarises the distribution of returns for one horizon within one
// group of trades (all trades, a single signal variant, one calendar year, …).
//
// N counts only legs whose horizon completed inside the available history, so
// N shrinks as the horizon lengthens near the end of the data.
type Stats struct {
	N        int
	Wins     int
	Losses   int
	Flat     int
	WinPct   float64
	Avg      float64 // mean return — the expectancy per trade
	Median   float64
	StdDev   float64
	Best     float64
	Worst    float64
	AvgWin   float64
	AvgLoss  float64 // reported as a negative number
	PayoffR  float64 // AvgWin / |AvgLoss| — the reward-to-risk ratio
	ProfitF  float64 // gross profit / gross loss
	Sharpe   float64 // Avg / StdDev — per-trade, not annualised
	AvgMFE   float64 // mean best unrealised move (upside actually available)
	AvgMAE   float64 // mean worst unrealised move (heat taken before the exit)
	TotalPct float64 // sum of returns — equity change at 1 unit per trade
}

// Group is a named bucket of trades with one Stats entry per horizon,
// index-aligned with the Result.Horizons slice.
type Group struct {
	Name    string
	Trades  int
	Longs   int
	Shorts  int
	PerHzn  []Stats
	SortKey float64 // primary-horizon average, used to rank groups in reports
}

// Summarize builds the full statistical view of a replay: the overall group,
// a group per signal variant, a group per direction, and a group per calendar
// year. primaryHorizon selects which horizon ranks the variant table.
func Summarize(res *Result, primaryHorizon int) (overall Group, byVariant, byDirection, byYear []Group) {
	if primaryHorizon < 0 || primaryHorizon >= len(res.Horizons) {
		primaryHorizon = 0
	}

	overall = buildGroup("ALL SIGNALS", res.Trades, res.Horizons, primaryHorizon)

	byVariant = groupBy(res, primaryHorizon, func(t Trade) string { return t.Variant })
	byDirection = groupBy(res, primaryHorizon, func(t Trade) string { return t.Direction.String() })
	byYear = groupBy(res, primaryHorizon, func(t Trade) string { return strconv.Itoa(t.EntryDate.Year()) })

	// Variants rank by edge (best average first); years read chronologically.
	sort.SliceStable(byVariant, func(i, j int) bool { return byVariant[i].SortKey > byVariant[j].SortKey })
	sort.SliceStable(byYear, func(i, j int) bool { return byYear[i].Name < byYear[j].Name })
	sort.SliceStable(byDirection, func(i, j int) bool { return byDirection[i].Name < byDirection[j].Name })
	return overall, byVariant, byDirection, byYear
}

// groupBy buckets trades by the key function and summarises each bucket.
func groupBy(res *Result, primary int, key func(Trade) string) []Group {
	buckets := make(map[string][]Trade)
	for _, t := range res.Trades {
		k := key(t)
		buckets[k] = append(buckets[k], t)
	}
	groups := make([]Group, 0, len(buckets))
	for name, trades := range buckets {
		groups = append(groups, buildGroup(name, trades, res.Horizons, primary))
	}
	return groups
}

// buildGroup computes per-horizon statistics for one bucket of trades.
func buildGroup(name string, trades []Trade, horizons []Horizon, primary int) Group {
	g := Group{Name: name, Trades: len(trades), PerHzn: make([]Stats, len(horizons))}
	for _, t := range trades {
		if t.Direction == Short {
			g.Shorts++
		} else {
			g.Longs++
		}
	}

	for h := range horizons {
		returns := make([]float64, 0, len(trades))
		var mfe, mae []float64
		for _, t := range trades {
			if h >= len(t.Legs) || !t.Legs[h].Valid {
				continue
			}
			returns = append(returns, t.Legs[h].ReturnPct)
			mfe = append(mfe, t.Legs[h].MaxFavPct)
			mae = append(mae, t.Legs[h].MaxAdvPct)
		}
		g.PerHzn[h] = computeStats(returns, mfe, mae)
	}
	if primary < len(g.PerHzn) {
		g.SortKey = g.PerHzn[primary].Avg
	}
	return g
}

// computeStats reduces a slice of returns to the full Stats record.
// An empty slice yields a zero Stats, which reports render as "—".
func computeStats(returns, mfe, mae []float64) Stats {
	s := Stats{N: len(returns)}
	if s.N == 0 {
		return s
	}

	var grossProfit, grossLoss, sumWin, sumLoss float64
	s.Best, s.Worst = returns[0], returns[0]
	for _, r := range returns {
		s.TotalPct += r
		switch {
		case r > 0:
			s.Wins++
			sumWin += r
			grossProfit += r
		case r < 0:
			s.Losses++
			sumLoss += r
			grossLoss += -r
		default:
			s.Flat++
		}
		if r > s.Best {
			s.Best = r
		}
		if r < s.Worst {
			s.Worst = r
		}
	}

	s.Avg = s.TotalPct / float64(s.N)
	// Win rate counts only decided trades in the numerator; exact-zero moves are
	// rare on daily closes but are neither a win nor a loss.
	s.WinPct = float64(s.Wins) / float64(s.N) * 100.0

	sorted := append([]float64(nil), returns...)
	sort.Float64s(sorted)
	if s.N%2 == 1 {
		s.Median = sorted[s.N/2]
	} else {
		s.Median = (sorted[s.N/2-1] + sorted[s.N/2]) / 2.0
	}

	if s.N > 1 {
		var sumSq float64
		for _, r := range returns {
			d := r - s.Avg
			sumSq += d * d
		}
		s.StdDev = math.Sqrt(sumSq / float64(s.N-1))
		if s.StdDev > 0 {
			s.Sharpe = s.Avg / s.StdDev
		}
	}

	if s.Wins > 0 {
		s.AvgWin = sumWin / float64(s.Wins)
	}
	if s.Losses > 0 {
		s.AvgLoss = sumLoss / float64(s.Losses) // negative by construction
		if s.AvgWin > 0 {
			s.PayoffR = s.AvgWin / math.Abs(s.AvgLoss)
		}
	}
	switch {
	case grossLoss > 0:
		s.ProfitF = grossProfit / grossLoss
	case grossProfit > 0:
		s.ProfitF = math.Inf(1) // every decided trade won
	}

	s.AvgMFE = mean(mfe)
	s.AvgMAE = mean(mae)
	return s
}

// mean returns the arithmetic mean, or 0 for an empty slice.
func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// TopTrades returns the n best (worst when best is false) trades ranked by the
// given horizon, ignoring trades whose leg never completed.
func TopTrades(trades []Trade, horizon, n int, best bool) []Trade {
	eligible := make([]Trade, 0, len(trades))
	for _, t := range trades {
		if horizon < len(t.Legs) && t.Legs[horizon].Valid {
			eligible = append(eligible, t)
		}
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		if best {
			return eligible[i].Legs[horizon].ReturnPct > eligible[j].Legs[horizon].ReturnPct
		}
		return eligible[i].Legs[horizon].ReturnPct < eligible[j].Legs[horizon].ReturnPct
	})
	if len(eligible) > n {
		eligible = eligible[:n]
	}
	return eligible
}
