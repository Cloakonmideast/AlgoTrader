// cmd/backtest/main.go
//
// Offline strategy backtester. Replays stored daily candles through the live
// strategy.Strategy implementations and reports forward returns and win rate
// at 1D / 5D / 1M / 3M holding horizons.
//
// Usage:
//
//	go run ./cmd/backtest                                   # Ichimoku, Nifty 500, full history
//	go run ./cmd/backtest -strategy rsi                      # RSI instead
//	go run ./cmd/backtest -strategy all -universe all        # every strategy, every token in the DB
//	go run ./cmd/backtest -from 2023-01-01 -cost 30          # windowed, 30 bps round-trip cost
//	go run ./cmd/backtest -horizons 1D:1,2W:10,6M:126        # custom holding periods
//
// Symbol names come from the Zerodha instrument master when KITE_API_KEY and
// KITE_ACCESS_TOKEN are set; the mapping is cached to -symbols so later runs
// work fully offline.
//
// Env vars: DATABASE_PATH, KITE_API_KEY (optional), KITE_ACCESS_TOKEN (optional)
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"

	"algotrader/internal/backtest"
	"algotrader/internal/service"
	"algotrader/internal/strategy"
)

// strategySpec binds a CLI name to a factory and the history each strategy
// needs before its first meaningful evaluation.
type strategySpec struct {
	Name    string
	Warmup  int
	Factory func() strategy.Strategy
}

func main() {
	var (
		dbPath      = flag.String("db", getenvOr("DATABASE_PATH", "market_data.db"), "Path to the SQLite candle database")
		stratNames  = flag.String("strategy", "ichimoku", "Strategy to test: ichimoku, rsi, or all (comma-separated)")
		rsiPeriod   = flag.Int("rsi-period", 14, "Lookback period for the RSI strategy")
		universe    = flag.String("universe", "nifty500", "Universe: nifty500 or all")
		limit       = flag.Int("limit", 0, "Cap the universe to N tokens (0 = no cap) — useful for quick runs")
		fromStr     = flag.String("from", "", "Only count signals on or after this date (YYYY-MM-DD)")
		toStr       = flag.String("to", "", "Only count signals on or before this date (YYYY-MM-DD)")
		horizonStr  = flag.String("horizons", "1D:1,5D:5,1M:21,3M:63", "Holding periods as LABEL:SESSIONS, comma-separated")
		primaryStr  = flag.String("primary", "1M", "Horizon label used to rank signals and list extremes")
		cost        = flag.Float64("cost", 0, "Round-trip cost in basis points, deducted from every return")
		cooldown    = flag.Int("cooldown", 0, "Suppress a repeat of the same signal on the same token for N sessions")
		stopSMA     = flag.Int("stop-sma", 0, "Exit on a CLOSE through the N-period SMA of closes — below it for longs, above for shorts (0 = no stop, hold the full horizon)")
		stopTrail   = flag.Bool("stop-trail", false, "Turn -stop-sma into a trailing stop: a close through the SMA arms a level at that session's low (high if short) instead of exiting; the trade runs until a later session breaks it")
		stopIntra   = flag.Bool("stop-intraday", false, "With -stop-trail, trigger on an intraday touch of the armed level and fill there (gaps fill at the open) rather than on a close beyond it")
		stopMaxLoss = flag.Float64("stop-max-loss", 0, "Hard floor under -stop-sma: a resting stop this many %% adverse to entry, always filled intraday (0 = none). Works standalone too")
		maxMove     = flag.Float64("max-move", 60, "Discard any leg whose absolute move exceeds this %% — filters unadjusted splits/bonuses (0 = keep all)")
		workers     = flag.Int("workers", 8, "Tokens replayed concurrently")
		warmup      = flag.Int("warmup", 0, "Override the minimum candles of history before evaluation starts")
		topN        = flag.Int("top", 10, "How many best/worst individual trades to list (0 = hide)")
		minSignals  = flag.Int("min-signals", 5, "Hide signal variants with fewer than N occurrences")
		outDir      = flag.String("out", "backtest_out", "Directory for the CSV and text report files (empty = console only)")
		symbolsCSV  = flag.String("symbols", "backtest_symbols.csv", "Token→symbol cache file")
		refreshSyms = flag.Bool("refresh-symbols", false, "Re-fetch the token→symbol map from Kite even if the cache exists")
		quiet       = flag.Bool("quiet", false, "Suppress the progress indicator")
		signalsOnly = flag.String("signals", "", "Keep only signals whose name contains one of these comma-separated fragments, e.g. \"(BUY),(Support)\"")
		confluence  = flag.Bool("confluence", false, "Also report setups where every selected strategy signals the same side (needs 2+ strategies)")
		confWindow  = flag.Int("confluence-window", 5, "Max sessions between the agreeing signals (0 = same session)")
		confCool    = flag.Int("confluence-cooldown", 5, "Sessions before the same token and side can requalify as confluence")
	)
	flag.Parse()

	horizons, err := parseHorizons(*horizonStr)
	if err != nil {
		fatal("%v", err)
	}
	primary := horizonIndex(horizons, *primaryStr)

	from, err := parseDate(*fromStr)
	if err != nil {
		fatal("-from: %v", err)
	}
	to, err := parseDate(*toStr)
	if err != nil {
		fatal("-to: %v", err)
	}
	if !to.IsZero() {
		to = to.Add(24*time.Hour - time.Nanosecond) // make -to inclusive of the whole day
	}

	specs, err := resolveStrategies(*stratNames, *rsiPeriod, *warmup)
	if err != nil {
		fatal("%v", err)
	}

	if _, err := os.Stat(*dbPath); err != nil {
		fatal("candle database not found at %q (set -db or DATABASE_PATH)", *dbPath)
	}

	// ── Universe + symbol resolution ─────────────────────────────────────────
	// The Kite instrument master is only fetched when the local cache is empty
	// or a refresh is asked for — a backtest should not depend on a live API.
	symbols := loadSymbolCache(*symbolsCSV)
	if len(symbols) > 0 && !*refreshSyms {
		info("using cached token→symbol map from %s (%d entries)", *symbolsCSV, len(symbols))
	} else {
		if fresh, ok := fetchSymbolsFromKite(); ok {
			symbols = fresh
			if err := saveSymbolCache(*symbolsCSV, symbols); err != nil {
				warn("could not cache symbols to %s: %v", *symbolsCSV, err)
			} else {
				info("cached %d token→symbol mappings to %s", len(symbols), *symbolsCSV)
			}
		} else if len(symbols) == 0 {
			warn("no symbol cache and no Kite credentials — reports will show raw token IDs")
		}
	}

	dbTokens, err := backtest.LoadUniverse(*dbPath)
	if err != nil {
		fatal("%v", err)
	}
	info("database %s holds %d instrument tokens", *dbPath, len(dbTokens))

	tokens, label := selectUniverse(*universe, dbTokens, symbols)
	if *limit > 0 && *limit < len(tokens) {
		tokens = tokens[:*limit]
		label = fmt.Sprintf("%s (capped at %d)", label, *limit)
	}
	if len(tokens) == 0 {
		fatal("universe is empty — nothing to backtest")
	}
	info("universe: %s → %d tokens\n", label, len(tokens))

	if *outDir != "" {
		if err := os.MkdirAll(*outDir, 0o755); err != nil {
			fatal("cannot create output directory %q: %v", *outDir, err)
		}
	}

	if *confluence && len(specs) < 2 {
		fatal("-confluence needs 2+ strategies — pass -strategy all (or a comma-separated list)")
	}

	signalFragments := splitFragments(*signalsOnly)

	opt := backtest.ReportOptions{
		Primary:       primary,
		TopN:          *topN,
		MinSignals:    *minSignals,
		ShowYears:     true,
		ShowDirection: true,
	}

	// ── Run each strategy ────────────────────────────────────────────────────
	results := make([]*backtest.Result, 0, len(specs))
	for _, spec := range specs {
		cfg := backtest.Config{
			DBPath:       *dbPath,
			Tokens:       tokens,
			Symbols:      symbols,
			Horizons:     horizons,
			Warmup:       spec.Warmup,
			From:         from,
			To:           to,
			CostBps:      *cost,
			MaxMovePct:   *maxMove,
			CooldownDays: *cooldown,
			StopSMA:      *stopSMA,
			StopTrailLow:   *stopTrail,
			StopIntraday:   *stopIntra,
			StopMaxLossPct: *stopMaxLoss,
			Workers:        *workers,
			NewStrategy:  spec.Factory,
		}
		if !*quiet {
			cfg.Progress = progressPrinter(spec.Name)
		}

		res, err := backtest.Run(cfg)
		if err != nil {
			fatal("backtest %s: %v", spec.Name, err)
		}
		if !*quiet {
			fmt.Fprintf(os.Stderr, "\r%s\r", strings.Repeat(" ", 70))
		}

		// The filter runs after the replay so stateful strategies still evolve
		// through every signal they would have fired live.
		if len(signalFragments) > 0 {
			before := len(res.Trades)
			note := fmt.Sprintf("Signal filter  : keeping only names containing %s", strings.Join(signalFragments, ", "))
			res = backtest.FilterTrades(res, note, func(t backtest.Trade) bool {
				return backtest.MatchesAny(t.Variant, signalFragments)
			})
			info("%s: signal filter kept %d of %d signals", spec.Name, len(res.Trades), before)
			if len(res.Trades) == 0 {
				warn("%s: no signals survived -signals — check the fragments match the variant names", spec.Name)
			}
		}

		// The stop changes what every number in the report means, so say so in the
		// header — with how often it actually bound at the primary horizon.
		if *stopSMA > 0 || *stopMaxLoss > 0 {
			var legs, stops, held int
			for _, t := range res.Trades {
				leg := t.Legs[primary]
				if !leg.Valid {
					continue
				}
				legs++
				held += leg.HeldDays
				if leg.Stopped {
					stops++
				}
			}
			var note string
			switch {
			case *stopSMA > 0 && *stopTrail:
				basis := "closes beyond"
				if *stopIntra {
					basis = "trades through"
				}
				note = fmt.Sprintf("Stop-loss      : a CLOSE through the %d-period SMA arms a stop at that session's\n"+
					"                   low (high if short); exit when a later session %s it", *stopSMA, basis)
			case *stopSMA > 0:
				note = fmt.Sprintf("Stop-loss      : exit on a CLOSE through the %d-period SMA (long: below, short: above)", *stopSMA)
			}
			if *stopMaxLoss > 0 {
				floor := fmt.Sprintf("Hard floor     : resting stop %.1f%% adverse to entry, filled intraday (gaps at the open)", *stopMaxLoss)
				if note == "" {
					note = floor
				} else {
					note += "\n  " + floor
				}
			}
			if legs > 0 {
				note += fmt.Sprintf("\n                   at %s, %.1f%% of legs stopped early; average hold %.1f of %d sessions",
					horizons[primary].Label,
					100*float64(stops)/float64(legs),
					float64(held)/float64(legs),
					horizons[primary].Days)
			}
			res.Notes = append(res.Notes, note)
		}

		results = append(results, res)
		backtest.Render(os.Stdout, res, opt)
		emit(*outDir, res, opt, primary)
	}

	// ── Confluence: where every strategy agrees on the same name and side ────
	if !*confluence {
		return
	}
	conf, err := backtest.Confluence(results, backtest.ConfluenceOptions{
		WindowSessions:   *confWindow,
		CooldownSessions: *confCool,
		RequireAll:       true,
	})
	if err != nil {
		fatal("%v", err)
	}
	backtest.Render(os.Stdout, conf, opt)
	emit(*outDir, conf, opt, primary)
}

// emit writes a result's report and CSVs into dir, unless dir is empty.
func emit(dir string, res *backtest.Result, opt backtest.ReportOptions, primary int) {
	if dir == "" {
		return
	}
	base := filepath.Join(dir, sanitize(res.StrategyName))
	writeReportFile(base+"_report.txt", res, opt)
	if err := backtest.WriteTradesCSV(base+"_trades.csv", res); err != nil {
		warn("%v", err)
	}
	if err := backtest.WriteSummaryCSV(base+"_summary.csv", res, primary); err != nil {
		warn("%v", err)
	}
	info("wrote %s_report.txt, %s_trades.csv, %s_summary.csv", base, base, base)
}

// resolveStrategies turns the -strategy flag into concrete specs.
// warmupOverride, when > 0, replaces each strategy's built-in warmup.
func resolveStrategies(names string, rsiPeriod, warmupOverride int) ([]strategySpec, error) {
	catalogue := map[string]strategySpec{
		// Ichimoku reaches back 103 sessions for the 52-shifted cloud and demands
		// 105 candles outright, so give it a little headroom.
		"ichimoku": {
			Name:    "ichimoku",
			Warmup:  110,
			Factory: func() strategy.Strategy { return strategy.NewIchimokuKinkoHyo() },
		},
		// RSI needs `period` closes for the seed average plus one extra candle for
		// the previous-RSI crossover check, plus the candle it consumes as "today".
		"rsi": {
			Name:    "rsi",
			Warmup:  rsiPeriod + 4,
			Factory: func() strategy.Strategy { return strategy.NewRSI(rsiPeriod) },
		},
		// Hilega Milega stacks a WMA(21) and an EMA(3) on top of an RSI(9), so it
		// needs the RSI seed plus a full WMA window of RSI values before the slow
		// line means anything. 60 gives that comfortable headroom.
		"hilega": {
			Name:    "hilega",
			Warmup:  60,
			Factory: func() strategy.Strategy { return strategy.NewHilegaMilega() },
		},
		// The same strategy with the short book switched off — see
		// HilegaMilega.LongOnly for why. Kept as a separate entry so the
		// two-sided results stay reproducible.
		"hilega-long": {
			Name:   "hilega-long",
			Warmup: 60,
			Factory: func() strategy.Strategy {
				h := strategy.NewHilegaMilega()
				h.LongOnly = true
				return h
			},
		},
	}

	var wanted []string
	for _, n := range strings.Split(names, ",") {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" {
			continue
		}
		if n == "all" {
			wanted = []string{"ichimoku", "rsi", "hilega"}
			break
		}
		wanted = append(wanted, n)
	}
	if len(wanted) == 0 {
		return nil, fmt.Errorf("-strategy: no strategy selected")
	}

	specs := make([]strategySpec, 0, len(wanted))
	for _, n := range wanted {
		spec, ok := catalogue[n]
		if !ok {
			return nil, fmt.Errorf("-strategy: unknown strategy %q (have: ichimoku, rsi, hilega, hilega-long, all)", n)
		}
		if warmupOverride > 0 {
			spec.Warmup = warmupOverride
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// selectUniverse narrows the DB token list to the requested universe.
// Nifty 500 filtering needs a symbol map; without one it degrades to the full
// DB universe rather than failing the run.
func selectUniverse(mode string, dbTokens []uint32, symbols map[uint32]string) ([]uint32, string) {
	if strings.EqualFold(mode, "all") {
		return dbTokens, "all tokens in DB"
	}

	if len(symbols) == 0 {
		warn("no token→symbol map available — cannot filter to Nifty 500; falling back to all DB tokens.")
		warn("set KITE_API_KEY and KITE_ACCESS_TOKEN once to build the cache, or pass -universe all.")
		return dbTokens, "all tokens in DB (Nifty 500 filter unavailable)"
	}

	index := service.EmbeddedNifty500Symbols()
	filtered := make([]uint32, 0, len(index))
	for _, tok := range dbTokens {
		if sym, ok := symbols[tok]; ok {
			if _, member := index[sym]; member {
				filtered = append(filtered, tok)
			}
		}
	}
	if len(filtered) == 0 {
		warn("no DB tokens matched the embedded Nifty 500 list; falling back to all DB tokens")
		return dbTokens, "all tokens in DB (no Nifty 500 matches)"
	}
	return filtered, "Nifty 500 constituents present in DB"
}

// fetchSymbolsFromKite builds a token→symbol map from the Zerodha instrument
// master. It returns ok=false (rather than failing) when credentials are absent
// or the API call errors, so the backtester stays usable offline.
func fetchSymbolsFromKite() (map[uint32]string, bool) {
	apiKey := os.Getenv("KITE_API_KEY")
	accessToken := os.Getenv("KITE_ACCESS_TOKEN")
	if strings.TrimSpace(apiKey) == "" || strings.TrimSpace(accessToken) == "" {
		return nil, false
	}

	kc := kiteconnect.New(apiKey)
	kc.SetAccessToken(accessToken)
	instruments, err := kc.GetInstrumentsByExchange("NSE")
	if err != nil {
		warn("Kite instrument master unavailable (%v) — falling back to the symbol cache", err)
		return nil, false
	}

	symbols := make(map[uint32]string, len(instruments))
	for _, inst := range instruments {
		if inst.InstrumentType == "EQ" && inst.Segment == "NSE" {
			symbols[uint32(inst.InstrumentToken)] = inst.Tradingsymbol
		}
	}
	if len(symbols) == 0 {
		return nil, false
	}
	return symbols, true
}

// loadSymbolCache reads a previously saved token,symbol CSV. A missing or
// malformed file yields an empty map — the caller treats that as "no symbols".
func loadSymbolCache(path string) map[uint32]string {
	f, err := os.Open(path)
	if err != nil {
		return map[uint32]string{}
	}
	defer f.Close()

	r := csv.NewReader(f)
	rows, err := r.ReadAll()
	if err != nil {
		warn("symbol cache %s unreadable: %v", path, err)
		return map[uint32]string{}
	}

	symbols := make(map[uint32]string, len(rows))
	for i, row := range rows {
		if i == 0 && len(row) > 0 && strings.EqualFold(row[0], "token") {
			continue // header
		}
		if len(row) < 2 {
			continue
		}
		tok, err := strconv.ParseUint(strings.TrimSpace(row[0]), 10, 32)
		if err != nil {
			continue
		}
		symbols[uint32(tok)] = strings.TrimSpace(row[1])
	}
	return symbols
}

// saveSymbolCache persists the token→symbol map so later runs need no API call.
func saveSymbolCache(path string, symbols map[uint32]string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("symbol cache: create %q: %w", path, err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{"token", "symbol"}); err != nil {
		return fmt.Errorf("symbol cache: write header: %w", err)
	}

	tokens := make([]uint32, 0, len(symbols))
	for tok := range symbols {
		tokens = append(tokens, tok)
	}
	sort.Slice(tokens, func(i, j int) bool { return tokens[i] < tokens[j] })

	for _, tok := range tokens {
		if err := w.Write([]string{strconv.FormatUint(uint64(tok), 10), symbols[tok]}); err != nil {
			return fmt.Errorf("symbol cache: write row: %w", err)
		}
	}
	return w.Error()
}

// parseHorizons parses "1D:1,5D:5,1M:21" into ordered Horizon values.
func parseHorizons(s string) ([]backtest.Horizon, error) {
	var horizons []backtest.Horizon
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		label, daysStr, ok := strings.Cut(part, ":")
		if !ok {
			return nil, fmt.Errorf("-horizons: %q is not in LABEL:SESSIONS form", part)
		}
		days, err := strconv.Atoi(strings.TrimSpace(daysStr))
		if err != nil || days <= 0 {
			return nil, fmt.Errorf("-horizons: %q has an invalid session count", part)
		}
		horizons = append(horizons, backtest.Horizon{Label: strings.TrimSpace(label), Days: days})
	}
	if len(horizons) == 0 {
		return nil, fmt.Errorf("-horizons: no valid horizons parsed")
	}
	return horizons, nil
}

// horizonIndex finds the horizon with the given label, defaulting to the last
// one when the label is unknown (the longest hold is the most informative default).
func horizonIndex(horizons []backtest.Horizon, label string) int {
	for i, h := range horizons {
		if strings.EqualFold(h.Label, label) {
			return i
		}
	}
	return len(horizons) - 1
}

// splitFragments parses the -signals flag into trimmed, non-empty fragments.
func splitFragments(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// parseDate accepts YYYY-MM-DD and returns the zero time for an empty string.
func parseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a YYYY-MM-DD date", s)
	}
	return t, nil
}

// progressPrinter returns a callback that redraws a single stderr status line.
func progressPrinter(name string) func(done, total int) {
	return func(done, total int) {
		if done%25 != 0 && done != total {
			return
		}
		fmt.Fprintf(os.Stderr, "\r  [%s] replaying %d/%d tokens…", name, done, total)
	}
}

// writeReportFile renders the same report that went to stdout into a text file.
func writeReportFile(path string, res *backtest.Result, opt backtest.ReportOptions) {
	f, err := os.Create(path)
	if err != nil {
		warn("cannot write report %q: %v", path, err)
		return
	}
	defer f.Close()
	backtest.Render(f, res, opt)
}

// sanitize makes a strategy name safe to use as a filename stem.
func sanitize(s string) string {
	repl := strings.NewReplacer(" ", "_", "/", "_", "\\", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_")
	return strings.ToLower(repl.Replace(s))
}

func getenvOr(key, fallback string) string {
	if v := os.Getenv(key); strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

func info(format string, args ...any) { fmt.Fprintf(os.Stderr, "[backtest] "+format+"\n", args...) }
func warn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[backtest] WARNING: "+format+"\n", args...)
}
func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[backtest] FATAL: "+format+"\n", args...)
	os.Exit(1)
}
