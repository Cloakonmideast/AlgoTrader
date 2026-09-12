package backtest

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// ReportOptions controls how much detail Render writes.
type ReportOptions struct {
	// Primary selects the horizon index used to rank variants and list extremes.
	Primary int
	// TopN is how many best/worst individual trades to list. 0 hides the section.
	TopN int
	// MinSignals hides variant/year rows backed by fewer signals than this,
	// keeping statistically meaningless rows out of the headline tables.
	MinSignals int
	// ShowYears and ShowDirection toggle the breakdown tables.
	ShowYears     bool
	ShowDirection bool
}

// DefaultReportOptions ranks by the 1M horizon and shows every breakdown.
var DefaultReportOptions = ReportOptions{Primary: 2, TopN: 10, MinSignals: 1, ShowYears: true, ShowDirection: true}

// Render writes the full human-readable report for a replay to w.
func Render(w io.Writer, res *Result, opt ReportOptions) {
	if opt.Primary >= len(res.Horizons) {
		opt.Primary = 0
	}
	overall, byVariant, byDirection, byYear := Summarize(res, opt.Primary)

	writeBanner(w, res.StrategyName)
	writeRunInfo(w, res, overall)

	if len(res.Trades) == 0 {
		fmt.Fprintln(w, "\nNo signals fired for this strategy over the selected universe and window.")
		fmt.Fprintln(w, "Check the warmup requirement, the date filters, and that the DB holds enough history.")
		return
	}

	writeHeadline(w, res, overall)
	writeGroupTable(w, "PER-SIGNAL BREAKDOWN — every variant the strategy emits", res, byVariant, opt)
	if opt.ShowDirection {
		writeGroupTable(w, "BY DIRECTION", res, byDirection, ReportOptions{Primary: opt.Primary})
	}
	if opt.ShowYears {
		writeGroupTable(w, "BY SIGNAL YEAR — consistency across regimes", res, byYear, ReportOptions{Primary: opt.Primary})
	}
	if opt.TopN > 0 {
		writeExtremes(w, res, opt)
	}
	writeFooter(w, res, overall, opt)
}

// reportWidth is the fixed column width every rule and box in the report uses.
const reportWidth = 112

// writeBanner prints the report title box.
func writeBanner(w io.Writer, strategyName string) {
	title := fmt.Sprintf("BACKTEST REPORT — %s", strategyName)
	fmt.Fprintf(w, "\n╔%s╗\n", strings.Repeat("═", reportWidth-2))
	fmt.Fprintf(w, "║ %-*s ║\n", reportWidth-4, truncate(title, reportWidth-4))
	fmt.Fprintf(w, "╚%s╝\n\n", strings.Repeat("═", reportWidth-2))
}

// writeRunInfo prints the run parameters and universe coverage.
func writeRunInfo(w io.Writer, res *Result, overall Group) {
	fmt.Fprintf(w, "  Universe       : %d tokens  (%d replayed, %d skipped — insufficient history)\n",
		res.Universe, res.Replayed, res.Skipped)
	fmt.Fprintf(w, "  Data range     : %s → %s\n", fmtDate(res.DataStart), fmtDate(res.DataEnd))
	fmt.Fprintf(w, "  Sessions eval'd: %s\n", commas(res.Sessions))
	fmt.Fprintf(w, "  Signals fired  : %s  (%s long / %s short)\n",
		commas(overall.Trades), commas(overall.Longs), commas(overall.Shorts))
	if !res.FirstSignal.IsZero() {
		fmt.Fprintf(w, "  Signal window  : %s → %s\n", fmtDate(res.FirstSignal), fmtDate(res.LastSignal))
	}
	fmt.Fprintf(w, "  Round-trip cost: %.1f bps (%.2f%% deducted from every return)\n", res.CostBps, res.CostBps/100.0)
	if res.MaxMovePct > 0 {
		fmt.Fprintf(w, "  Outlier filter : |move| > %.0f%% discarded — %s leg(s) dropped (likely unadjusted splits/bonuses)\n",
			res.MaxMovePct, commas(res.Outliers))
	}
	if res.Elapsed > 0 {
		fmt.Fprintf(w, "  Runtime        : %s\n", res.Elapsed.Round(time.Millisecond))
	}
	for _, note := range res.Notes {
		fmt.Fprintf(w, "  %s\n", note)
	}
}

// writeHeadline prints the primary table: one row per holding horizon.
func writeHeadline(w io.Writer, res *Result, overall Group) {
	section(w, "HEADLINE — forward returns by holding period (entry at signal-day close)")

	fmt.Fprintf(w, "  %-8s %7s %8s %9s %9s %9s %9s %7s %7s %7s %9s %9s\n",
		"HORIZON", "N", "WIN%", "AVG%", "MEDIAN%", "AVGWIN%", "AVGLOSS%", "PF", "PAYOFF", "SHARPE", "BEST%", "WORST%")
	fmt.Fprintf(w, "  %s\n", strings.Repeat("─", reportWidth))

	for h, hz := range res.Horizons {
		s := overall.PerHzn[h]
		if s.N == 0 {
			fmt.Fprintf(w, "  %-8s %7s %8s   — insufficient forward data\n", hz.Label, "0", "—")
			continue
		}
		fmt.Fprintf(w, "  %-8s %7s %8s %9s %9s %9s %9s %7s %7s %7s %9s %9s\n",
			fmt.Sprintf("%s(%d)", hz.Label, hz.Days),
			commas(s.N),
			pct(s.WinPct),
			signed(s.Avg),
			signed(s.Median),
			signed(s.AvgWin),
			signed(s.AvgLoss),
			ratio(s.ProfitF),
			ratio(s.PayoffR),
			ratio(s.Sharpe),
			signed(s.Best),
			signed(s.Worst),
		)
	}

	// Excursion table — how much of the move was actually available intra-hold.
	fmt.Fprintf(w, "\n  %-8s %11s %11s %11s   %s\n", "HORIZON", "AVG MFE%", "AVG MAE%", "AVG NET%", "(MFE = best unrealised, MAE = worst unrealised)")
	fmt.Fprintf(w, "  %s\n", strings.Repeat("─", reportWidth))
	for h, hz := range res.Horizons {
		s := overall.PerHzn[h]
		if s.N == 0 {
			continue
		}
		fmt.Fprintf(w, "  %-8s %11s %11s %11s\n", hz.Label, signed(s.AvgMFE), signed(s.AvgMAE), signed(s.Avg))
	}
}

// writeGroupTable renders one breakdown table: a row per group, with an
// N / WIN% / AVG% block per horizon.
func writeGroupTable(w io.Writer, title string, res *Result, groups []Group, opt ReportOptions) {
	visible := make([]Group, 0, len(groups))
	hidden := 0
	for _, g := range groups {
		if g.Trades < opt.MinSignals {
			hidden++
			continue
		}
		visible = append(visible, g)
	}
	if len(visible) == 0 {
		return
	}

	section(w, title)

	const groupHeader = "SIGNAL / GROUP"
	nameW := len(groupHeader)
	for _, g := range visible {
		if len(g.Name) > nameW {
			nameW = len(g.Name)
		}
	}
	if nameW > 38 {
		nameW = 38
	}

	// Header row 1: horizon labels spanning each block.
	fmt.Fprintf(w, "  %-*s %7s", nameW, "", "")
	for _, hz := range res.Horizons {
		fmt.Fprintf(w, " │%s", center(hz.Label, 22))
	}
	fmt.Fprintln(w)

	// Header row 2: the columns inside each block.
	fmt.Fprintf(w, "  %-*s %7s", nameW, groupHeader, "TOTAL")
	for range res.Horizons {
		fmt.Fprintf(w, " │%6s %7s %7s", "N", "WIN%", "AVG%")
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s\n", strings.Repeat("─", nameW+8+len(res.Horizons)*24))

	for _, g := range visible {
		fmt.Fprintf(w, "  %-*s %7s", nameW, truncate(g.Name, nameW), commas(g.Trades))
		for h := range res.Horizons {
			s := g.PerHzn[h]
			if s.N == 0 {
				fmt.Fprintf(w, " │%6s %7s %7s", "—", "—", "—")
				continue
			}
			fmt.Fprintf(w, " │%6s %7s %7s", commas(s.N), pct(s.WinPct), signed(s.Avg))
		}
		fmt.Fprintln(w)
	}
	if hidden > 0 {
		fmt.Fprintf(w, "  (%d group(s) hidden — fewer than %d signals)\n", hidden, opt.MinSignals)
	}
}

// writeExtremes lists the best and worst individual trades at the primary horizon.
func writeExtremes(w io.Writer, res *Result, opt ReportOptions) {
	hz := res.Horizons[opt.Primary]
	best := TopTrades(res.Trades, opt.Primary, opt.TopN, true)
	worst := TopTrades(res.Trades, opt.Primary, opt.TopN, false)
	if len(best) == 0 {
		return
	}

	writeTradeList(w, fmt.Sprintf("BEST %d TRADES @ %s", len(best), hz.Label), best, opt.Primary)
	writeTradeList(w, fmt.Sprintf("WORST %d TRADES @ %s", len(worst), hz.Label), worst, opt.Primary)
}

// writeTradeList renders a small table of individual trades.
func writeTradeList(w io.Writer, title string, trades []Trade, h int) {
	section(w, title)
	fmt.Fprintf(w, "  %-16s %-12s %-6s %10s %10s %-12s %9s  %s\n",
		"SYMBOL", "ENTRY DATE", "SIDE", "ENTRY", "EXIT", "EXIT DATE", "RETURN%", "SIGNAL")
	fmt.Fprintf(w, "  %s\n", strings.Repeat("─", reportWidth))
	for _, t := range trades {
		leg := t.Legs[h]
		fmt.Fprintf(w, "  %-16s %-12s %-6s %10.2f %10.2f %-12s %9s  %s\n",
			truncate(t.Symbol, 16), fmtDate(t.EntryDate), t.Direction.String(),
			t.EntryPrice, leg.ExitPrice, fmtDate(leg.ExitDate), signed(leg.ReturnPct),
			truncate(t.Variant, 34))
	}
}

// writeFooter states the verdict at the primary horizon plus the standing caveats.
func writeFooter(w io.Writer, res *Result, overall Group, opt ReportOptions) {
	hz := res.Horizons[opt.Primary]
	s := overall.PerHzn[opt.Primary]

	section(w, "VERDICT")
	if s.N == 0 {
		fmt.Fprintf(w, "  No completed %s holds — the data ends before the horizon closes.\n", hz.Label)
		return
	}
	fmt.Fprintf(w, "  Of %s signals held for %s, %s%% won — average %s%% per trade,\n",
		commas(s.N), hz.Label, pct(s.WinPct), signed(s.Avg))
	fmt.Fprintf(w, "  median %s%%, profit factor %s, payoff ratio %s, per-trade Sharpe %s.\n",
		signed(s.Median), ratio(s.ProfitF), ratio(s.PayoffR), ratio(s.Sharpe))

	fmt.Fprintln(w, "\n  Caveats — read these before sizing anything on the numbers above:")
	if res.StopSMA > 0 || res.StopMaxLoss > 0 {
		trigger := fmt.Sprintf("a close through the %d-SMA", res.StopSMA)
		if res.StopTrailLow {
			basis := "a close beyond"
			if res.StopIntraday {
				basis = "a touch of"
			}
			trigger = fmt.Sprintf("%s the level armed at the low of each\n     session closing through the %d-SMA", basis, res.StopSMA)
		}
		if res.StopMaxLoss > 0 {
			hard := fmt.Sprintf("a resting stop %.1f%% adverse to entry", res.StopMaxLoss)
			if res.StopSMA > 0 {
				trigger += ", or " + hard
			} else {
				trigger = hard
			}
		}
		fmt.Fprintf(w, "   • Entry is the signal session's CLOSE; exit is the earlier of the horizon\n")
		fmt.Fprintf(w, "     and %s. No targets, sizing, or compounding.\n", trigger)
	} else {
		fmt.Fprintln(w, "   • Entry is the signal session's CLOSE and exit is a fixed-horizon close;")
		fmt.Fprintln(w, "     no stops, no targets, no position sizing, no compounding.")
	}
	fmt.Fprintln(w, "   • Every signal is taken. Live, StateRegistry caps one alert per token per day,")
	fmt.Fprintln(w, "     and capital limits how many concurrent positions you can actually hold.")
	fmt.Fprintln(w, "   • Live signals fire intraday on ticks; this replay fires at the close, so")
	fmt.Fprintln(w, "     same-day fills will differ.")
	fmt.Fprintln(w, "   • The universe is whatever the DB holds today — delisted names that were")
	fmt.Fprintln(w, "     purged are absent, which biases results upward (survivorship).")
	fmt.Fprintln(w, "   • Prices are not split/bonus adjusted unless the source data already was.")
	fmt.Fprintln(w)
}

// ── Formatting helpers ────────────────────────────────────────────────────────

// section prints a titled separator line.
func section(w io.Writer, title string) {
	fmt.Fprintf(w, "\n── %s ", title)
	pad := reportWidth - len(title) - 4
	if pad < 3 {
		pad = 3
	}
	fmt.Fprintf(w, "%s\n\n", strings.Repeat("─", pad))
}

// signed renders a percentage with an explicit sign, e.g. "+1.24" / "-0.87".
func signed(v float64) string {
	if math.IsNaN(v) {
		return "—"
	}
	return fmt.Sprintf("%+.2f", v)
}

// pct renders an unsigned percentage, e.g. "54.10".
func pct(v float64) string { return fmt.Sprintf("%.2f", v) }

// ratio renders a dimensionless ratio, collapsing infinity to "∞".
func ratio(v float64) string {
	if math.IsInf(v, 1) {
		return "∞"
	}
	if math.IsNaN(v) || v == 0 {
		return "—"
	}
	return fmt.Sprintf("%.2f", v)
}

// fmtDate renders a date as YYYY-MM-DD, or "—" when zero.
func fmtDate(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("2006-01-02")
}

// commas renders an integer with thousands separators.
func commas(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// truncate shortens s to width, marking elision with a trailing "…".
func truncate(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width <= 1 {
		return string(r[:width])
	}
	return string(r[:width-1]) + "…"
}

// center pads s with spaces so it sits in the middle of a width-wide field.
func center(s string, width int) string {
	if len(s) >= width {
		return s
	}
	left := (width - len(s)) / 2
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", width-len(s)-left)
}

// ── CSV export ────────────────────────────────────────────────────────────────

// WriteTradesCSV writes one row per signal with its outcome at every horizon,
// so the raw results can be re-analysed in Excel, pandas, or R.
func WriteTradesCSV(path string, res *Result) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("backtest: create %q: %w", path, err)
	}
	defer f.Close()

	cw := csv.NewWriter(f)
	defer cw.Flush()

	header := []string{"symbol", "token", "strategy", "signal", "direction", "entry_date", "entry_price"}
	for _, hz := range res.Horizons {
		l := strings.ToLower(hz.Label)
		header = append(header,
			l+"_exit_date", l+"_exit_price", l+"_return_pct", l+"_mfe_pct", l+"_mae_pct",
			l+"_exit_reason", l+"_held_days")
	}
	if err := cw.Write(header); err != nil {
		return fmt.Errorf("backtest: write header to %q: %w", path, err)
	}

	for _, t := range res.Trades {
		row := []string{
			t.Symbol,
			strconv.FormatUint(uint64(t.Token), 10),
			t.Strategy,
			t.Variant,
			t.Direction.String(),
			fmtDate(t.EntryDate),
			strconv.FormatFloat(t.EntryPrice, 'f', 2, 64),
		}
		for h := range res.Horizons {
			leg := t.Legs[h]
			if !leg.Valid {
				row = append(row, "", "", "", "", "", "", "")
				continue
			}
			reason := "horizon"
			if leg.Stopped {
				reason = "stop"
			}
			row = append(row,
				fmtDate(leg.ExitDate),
				strconv.FormatFloat(leg.ExitPrice, 'f', 2, 64),
				strconv.FormatFloat(leg.ReturnPct, 'f', 4, 64),
				strconv.FormatFloat(leg.MaxFavPct, 'f', 4, 64),
				strconv.FormatFloat(leg.MaxAdvPct, 'f', 4, 64),
				reason,
				strconv.Itoa(leg.HeldDays),
			)
		}
		if err := cw.Write(row); err != nil {
			return fmt.Errorf("backtest: write row to %q: %w", path, err)
		}
	}
	return cw.Error()
}

// WriteSummaryCSV writes every aggregate statistic — overall, per variant, per
// direction, per year — as a long-format table, one row per (group, horizon).
func WriteSummaryCSV(path string, res *Result, primary int) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("backtest: create %q: %w", path, err)
	}
	defer f.Close()

	cw := csv.NewWriter(f)
	defer cw.Flush()

	if err := cw.Write([]string{
		"group_type", "group", "horizon", "horizon_days", "signals", "n",
		"win_pct", "wins", "losses", "avg_pct", "median_pct", "stddev_pct",
		"avg_win_pct", "avg_loss_pct", "profit_factor", "payoff_ratio", "sharpe",
		"best_pct", "worst_pct", "avg_mfe_pct", "avg_mae_pct", "total_pct",
	}); err != nil {
		return fmt.Errorf("backtest: write header to %q: %w", path, err)
	}

	overall, byVariant, byDirection, byYear := Summarize(res, primary)
	sets := []struct {
		kind   string
		groups []Group
	}{
		{"overall", []Group{overall}},
		{"signal", byVariant},
		{"direction", byDirection},
		{"year", byYear},
	}

	for _, set := range sets {
		for _, g := range set.groups {
			for h, hz := range res.Horizons {
				s := g.PerHzn[h]
				row := []string{
					set.kind, g.Name, hz.Label, strconv.Itoa(hz.Days),
					strconv.Itoa(g.Trades), strconv.Itoa(s.N),
					f4(s.WinPct), strconv.Itoa(s.Wins), strconv.Itoa(s.Losses),
					f4(s.Avg), f4(s.Median), f4(s.StdDev),
					f4(s.AvgWin), f4(s.AvgLoss), f4(s.ProfitF), f4(s.PayoffR), f4(s.Sharpe),
					f4(s.Best), f4(s.Worst), f4(s.AvgMFE), f4(s.AvgMAE), f4(s.TotalPct),
				}
				if err := cw.Write(row); err != nil {
					return fmt.Errorf("backtest: write row to %q: %w", path, err)
				}
			}
		}
	}
	return cw.Error()
}

// f4 formats a float for CSV, rendering non-finite values as empty cells.
func f4(v float64) string {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return ""
	}
	return strconv.FormatFloat(v, 'f', 4, 64)
}
