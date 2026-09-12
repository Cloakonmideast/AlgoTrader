package backtest

import (
	"fmt"
	"sort"
	"strings"
)

// ConfluenceOptions controls how agreement between strategies is detected.
type ConfluenceOptions struct {
	// WindowSessions is how many trading sessions may separate the signals.
	// 0 means every strategy must fire on the SAME session; 5 means the others
	// must have fired within the previous trading week.
	WindowSessions int

	// CooldownSessions suppresses another confluence event on the same token and
	// side until this many sessions have passed. Without it, a cluster of daily
	// re-fires by both strategies produces near-identical overlapping entries.
	CooldownSessions int

	// RequireAll demands that EVERY supplied strategy agrees. When false, any
	// two agreeing strategies are enough (only meaningful with 3+ strategies).
	RequireAll bool
}

// DefaultConfluenceOptions requires agreement within one trading week and holds
// off for a week before counting the same setup again.
var DefaultConfluenceOptions = ConfluenceOptions{WindowSessions: 5, CooldownSessions: 5, RequireAll: true}

// Confluence finds instruments where several strategies independently signalled
// the SAME side within a short window, and returns them as a Result that reports
// exactly like a single-strategy run.
//
// Entry is the LAST of the agreeing signals — the earliest moment a trader could
// actually have known that both strategies were aligned. That trade's forward
// legs are reused verbatim, so the confluence numbers are directly comparable to
// the standalone reports.
//
// Signals where the strategies disagree on direction are never counted; they are
// tallied and reported as conflicts instead.
func Confluence(results []*Result, opt ConfluenceOptions) (*Result, error) {
	if len(results) < 2 {
		return nil, fmt.Errorf("backtest: confluence needs at least 2 strategy results, got %d", len(results))
	}
	if opt.WindowSessions < 0 {
		opt.WindowSessions = 0
	}

	names := make([]string, 0, len(results))
	for _, r := range results {
		if r == nil {
			return nil, fmt.Errorf("backtest: confluence received a nil result")
		}
		names = append(names, r.StrategyName)
	}
	if err := sameHorizons(results); err != nil {
		return nil, err
	}

	// Index every trade by token, then by strategy, in session order.
	byToken := make(map[uint32]map[string][]Trade)
	for _, r := range results {
		for _, t := range r.Trades {
			perStrategy, ok := byToken[t.Token]
			if !ok {
				perStrategy = make(map[string][]Trade, len(results))
				byToken[t.Token] = perStrategy
			}
			perStrategy[r.StrategyName] = append(perStrategy[r.StrategyName], t)
		}
	}
	for _, perStrategy := range byToken {
		for _, trades := range perStrategy {
			sort.SliceStable(trades, func(i, j int) bool { return trades[i].SessionIdx < trades[j].SessionIdx })
		}
	}

	var (
		events    []Trade
		conflicts int
	)

	for token, perStrategy := range byToken {
		if opt.RequireAll && len(perStrategy) < len(results) {
			continue // this token never fired on at least one strategy
		}

		// Candidate anchors: every trade, considered as the LAST signal to arrive.
		var anchors []Trade
		for _, trades := range perStrategy {
			anchors = append(anchors, trades...)
		}
		sort.SliceStable(anchors, func(i, j int) bool {
			if anchors[i].SessionIdx != anchors[j].SessionIdx {
				return anchors[i].SessionIdx < anchors[j].SessionIdx
			}
			return anchors[i].Variant < anchors[j].Variant
		})

		// lastEmitted[direction] is the session of the previous accepted event,
		// used to enforce the cooldown per side.
		lastEmitted := map[Direction]int{}
		emittedAt := map[Direction]map[int]bool{Long: {}, Short: {}}

		for _, anchor := range anchors {
			partners, conflicted := matchPartners(perStrategy, anchor, opt)
			if conflicted {
				conflicts++
			}
			required := len(results) - 1
			if !opt.RequireAll {
				required = 1
			}
			if len(partners) < required {
				continue
			}

			// Two strategies firing on the same session each anchor the same
			// event — keep it once.
			if emittedAt[anchor.Direction][anchor.SessionIdx] {
				continue
			}
			if prev, seen := lastEmitted[anchor.Direction]; seen &&
				opt.CooldownSessions > 0 && anchor.SessionIdx-prev < opt.CooldownSessions {
				continue
			}
			lastEmitted[anchor.Direction] = anchor.SessionIdx
			emittedAt[anchor.Direction][anchor.SessionIdx] = true

			events = append(events, buildEvent(token, anchor, partners))
		}
	}

	out := &Result{
		StrategyName: "Confluence: " + strings.Join(names, " + "),
		Horizons:     results[0].Horizons,
		Trades:       events,
		Universe:     results[0].Universe,
		Replayed:     results[0].Replayed,
		Skipped:      results[0].Skipped,
		CostBps:      results[0].CostBps,
		MaxMovePct:   results[0].MaxMovePct,
		DataStart:    results[0].DataStart,
		DataEnd:      results[0].DataEnd,
	}
	for _, r := range results {
		out.Sessions += r.Sessions
		out.Outliers += r.Outliers
		trackSpan(&out.DataStart, &out.DataEnd, r.DataStart, r.DataEnd)
	}

	window := "the same session"
	if opt.WindowSessions > 0 {
		window = fmt.Sprintf("%d session(s) of each other", opt.WindowSessions)
	}
	out.Notes = append(out.Notes,
		fmt.Sprintf("Agreement rule : %s must all signal the SAME side within %s", strings.Join(names, " and "), window),
		"Entry          : the LAST agreeing signal — the first moment both were known to align",
		fmt.Sprintf("Cooldown       : %d session(s) before the same token and side can requalify", opt.CooldownSessions),
		fmt.Sprintf("Conflicts      : %s setup(s) where the strategies fired within the window on OPPOSITE sides (excluded)", commas(conflicts)),
		fmt.Sprintf("Mean align gap : %.1f session(s) between the first and last agreeing signal", meanAlignGap(events)),
	)

	sort.SliceStable(out.Trades, func(i, j int) bool {
		if !out.Trades[i].EntryDate.Equal(out.Trades[j].EntryDate) {
			return out.Trades[i].EntryDate.Before(out.Trades[j].EntryDate)
		}
		return out.Trades[i].Symbol < out.Trades[j].Symbol
	})
	if len(out.Trades) > 0 {
		out.FirstSignal = out.Trades[0].EntryDate
		out.LastSignal = out.Trades[len(out.Trades)-1].EntryDate
	}
	return out, nil
}

// matchPartners finds, for every strategy other than the anchor's, the most
// recent same-side signal that lands inside the window at or before the anchor.
//
// It also reports whether some other strategy fired inside the window on the
// OPPOSITE side, which marks the setup as a disagreement rather than a miss.
func matchPartners(perStrategy map[string][]Trade, anchor Trade, opt ConfluenceOptions) (partners []Trade, conflicted bool) {
	for strategyName, trades := range perStrategy {
		if strategyName == anchor.Strategy {
			continue
		}
		var best *Trade
		sawOpposite := false
		for i := range trades {
			t := trades[i]
			gap := anchor.SessionIdx - t.SessionIdx
			if gap < 0 || gap > opt.WindowSessions {
				continue
			}
			if t.Direction != anchor.Direction {
				sawOpposite = true
				continue
			}
			if best == nil || t.SessionIdx > best.SessionIdx {
				best = &trades[i]
			}
		}
		if best != nil {
			// An agreeing signal outweighs an opposite one from the same strategy.
			partners = append(partners, *best)
			continue
		}
		if sawOpposite {
			conflicted = true
		}
	}
	return partners, conflicted
}

// buildEvent turns an anchor plus its agreeing partners into a single Trade.
// The anchor's legs are reused because the anchor IS the entry.
func buildEvent(token uint32, anchor Trade, partners []Trade) Trade {
	contributors := make([]string, 0, len(partners)+1)
	contributors = append(contributors, compactVariant(anchor))
	for _, p := range partners {
		contributors = append(contributors, compactVariant(p))
	}
	sort.Strings(contributors)

	// Gap = how many sessions separated the first and last agreeing signal.
	earliest := anchor.SessionIdx
	for _, p := range partners {
		if p.SessionIdx < earliest {
			earliest = p.SessionIdx
		}
	}

	return Trade{
		Strategy:   "Confluence",
		Variant:    strings.Join(contributors, "  +  "),
		Token:      token,
		Symbol:     anchor.Symbol,
		Direction:  anchor.Direction,
		EntryDate:  anchor.EntryDate,
		EntryPrice: anchor.EntryPrice,
		SessionIdx: anchor.SessionIdx,
		Legs:       anchor.Legs,
		AlignGap:   anchor.SessionIdx - earliest,
	}
}

// compactVariant shortens "IchimokuKinkoHyo (BUY) Gold Cross" to
// "ICH (BUY) Gold Cross" so a combined name still fits the report's name column.
// Confluence variants pair two of these, and the full form overflows.
func compactVariant(t Trade) string {
	tag := strings.ToUpper(t.Strategy)
	if len(tag) > 3 {
		tag = tag[:3]
	}
	detail := strings.TrimSpace(strings.TrimPrefix(t.Variant, t.Strategy))
	if detail == "" {
		return tag
	}
	return tag + " " + detail
}

// sameHorizons rejects results that were not measured over identical horizons,
// since their legs would not be comparable.
func sameHorizons(results []*Result) error {
	ref := results[0].Horizons
	for _, r := range results[1:] {
		if len(r.Horizons) != len(ref) {
			return fmt.Errorf("backtest: confluence requires identical horizons across strategies")
		}
		for i := range ref {
			if r.Horizons[i].Days != ref[i].Days || r.Horizons[i].Label != ref[i].Label {
				return fmt.Errorf("backtest: confluence requires identical horizons across strategies")
			}
		}
	}
	return nil
}

// meanAlignGap is the average number of sessions between the first and last
// agreeing signal. A low number means the strategies tend to fire together
// rather than one consistently lagging the other.
func meanAlignGap(trades []Trade) float64 {
	if len(trades) == 0 {
		return 0
	}
	var sum int
	for _, t := range trades {
		sum += t.AlignGap
	}
	return float64(sum) / float64(len(trades))
}
