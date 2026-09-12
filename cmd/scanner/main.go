// cmd/scanner/main.go
//
// Offline Ichimoku scanner — Nifty 500 or full NSE.
// Reads candle history from SQLite, applies the full 6-condition strategy
// (including cloud cross-out filter), and prints all matching stocks.
//
// Usage:
//
//	go run ./cmd/scanner          # Nifty 500 only (default)
//	go run ./cmd/scanner -all     # every token in the DB
//
// Env vars: KITE_API_KEY, KITE_ACCESS_TOKEN, DATABASE_PATH
package main

import (
	"database/sql"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	kiteconnect "github.com/zerodha/gokiteconnect/v4"
)

// Result holds the scanner output for one matching stock.
type Result struct {
	Symbol    string
	Close     float64
	Conv      float64
	Base      float64
	CloudTop  float64
	CloudBot  float64
	TodayOpen float64
	YestClose float64
	Scenario  string // "INTRADAY" or "GAP_OPEN"
}

const (
	nifty500URL = "https://archives.nseindia.com/content/indices/ind_nifty500list.csv"
	minCandles  = 105
)

func main() {
	log.SetFlags(log.Ltime | log.Lshortfile)

	scanAll := flag.Bool("all", false, "Scan every token in the DB instead of Nifty 500 only")
	flag.Parse()

	if *scanAll {
		fmt.Print(`
╔══════════════════════════════════════════════════════════╗
║      ALGOTRADER — Full NSE Ichimoku Scanner              ║
║   All 6 conditions incl. cloud cross-out filter          ║
╚══════════════════════════════════════════════════════════╝
`)
	} else {
		fmt.Print(`
╔══════════════════════════════════════════════════════════╗
║      ALGOTRADER — Nifty 500 Ichimoku Scanner             ║
║   All 6 conditions incl. cloud cross-out filter          ║
╚══════════════════════════════════════════════════════════╝
`)
	}

	apiKey      := mustEnv("KITE_API_KEY")
	accessToken := mustEnv("KITE_ACCESS_TOKEN")
	dbPath      := getenvOr("DATABASE_PATH", "market_data.db")

	// ── 1. Build token→symbol map from Zerodha instrument master ─────────────
	fmt.Println("[scanner] Fetching Zerodha NSE instrument master...")
	kc := kiteconnect.New(apiKey)
	kc.SetAccessToken(accessToken)

	instruments, err := kc.GetInstrumentsByExchange("NSE")
	if err != nil {
		log.Fatalf("[scanner] GetInstruments failed: %v", err)
	}

	// ── 2. Open SQLite ────────────────────────────────────────────────────────
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?mode=ro&_journal_mode=WAL", dbPath))
	if err != nil {
		log.Fatalf("[scanner] Cannot open DB: %v", err)
	}
	defer db.Close()
	fmt.Printf("[scanner] Opened: %s\n", dbPath)

	// ── 3. Determine which tokens to scan ─────────────────────────────────────
	tokenToSymbol := make(map[uint32]string, 5000)
	var scanTokens map[uint32]bool

	if *scanAll {
		// Full NSE scan: use every token present in the DB.
		// Build a full token→symbol map from the instrument master first.
		for _, inst := range instruments {
			if inst.InstrumentType == "EQ" && inst.Segment == "NSE" {
				tokenToSymbol[uint32(inst.InstrumentToken)] = inst.Tradingsymbol
			}
		}
		dbTokens, err := loadAllTokens(db)
		if err != nil {
			log.Fatalf("[scanner] Failed to load tokens from DB: %v", err)
		}
		scanTokens = dbTokens
		fmt.Printf("[scanner] Tokens in DB: %d\n\n", len(scanTokens))
	} else {
		// Nifty 500 only: download the constituent list and filter.
		fmt.Println("[scanner] Fetching Nifty 500 constituent list from NSE India...")
		nifty500, err := downloadNifty500Symbols()
		if err != nil {
			log.Fatalf("[scanner] Failed to fetch Nifty 500 list: %v", err)
		}
		fmt.Printf("[scanner] Nifty 500 CSV: %d symbols\n", len(nifty500))

		nifty500Tokens := make(map[uint32]bool, 500)
		for _, inst := range instruments {
			if inst.InstrumentType != "EQ" || inst.Segment != "NSE" {
				continue
			}
			if _, ok := nifty500[inst.Tradingsymbol]; ok {
				tok := uint32(inst.InstrumentToken)
				tokenToSymbol[tok] = inst.Tradingsymbol
				nifty500Tokens[tok] = true
			}
		}
		fmt.Printf("[scanner] Nifty 500 tokens mapped: %d\n\n", len(nifty500Tokens))
		scanTokens = nifty500Tokens
	}

	// ── 4. For each token, load candles and evaluate ─────────────────────────
	var results []Result
	skipped := 0

	for token := range scanTokens {
		candles, err := loadCandles(db, token)
		if err != nil || len(candles) < minCandles {
			skipped++
			continue
		}

		// Use last closed candle as "today" (offline mode — no live ticks)
		last    := candles[len(candles)-1]
		prev    := candles[len(candles)-2]  // the session before last

		currentPrice  := last.Close
		todayOpen     := last.Open
		todayHigh     := last.High
		todayLow      := last.Low
		yesterdayClose := prev.Close

		cnt := 26

		// Conversion Line (9-period)
		convCandles := candles[len(candles)-9:]
		convLine    := periodMidpoint(convCandles, todayHigh, todayLow)

		// Base Line (26-period)
		baseCandles := candles[len(candles)-26:]
		baseLine    := periodMidpoint(baseCandles, todayHigh, todayLow)

		// Leading Span A (shifted back 26)
		sess26         := candles[len(candles)-cnt]
		pastConv       := periodMidpoint(candles[len(candles)-(cnt+8):len(candles)-cnt], sess26.High, sess26.Low)
		pastBase       := periodMidpoint(candles[len(candles)-(cnt+25):len(candles)-cnt], sess26.High, sess26.Low)
		leadingSpanA   := (pastConv + pastBase) / 2.0

		// Leading Span A (shifted back 52)
		sess52         := candles[len(candles)-(cnt+26)]
		pastConv52     := periodMidpoint(candles[len(candles)-(cnt+26+8):len(candles)-(cnt+26)], sess52.High, sess52.Low)
		pastBase52     := periodMidpoint(candles[len(candles)-(cnt+26+25):len(candles)-(cnt+26)], sess52.High, sess52.Low)
		leadingSpanA52 := (pastConv52 + pastBase52) / 2.0

		// Leading Span B (shifted back 26, 52-period window)
		leadingSpanB   := periodMidpoint(candles[len(candles)-(cnt+51):len(candles)-cnt], sess26.High, sess26.Low)
		leadingSpanB52 := periodMidpoint(candles[len(candles)-(cnt+26+51):len(candles)-(cnt+26)], sess52.High, sess52.Low)

		// Lagging Span reference price
		priceFrom26 := candles[len(candles)-(cnt+1)].High

		// Cloud boundaries (current)
		cloudTop := leadingSpanA
		cloudBot := leadingSpanB
		if leadingSpanB > leadingSpanA {
			cloudTop = leadingSpanB
			cloudBot = leadingSpanA
		}

		// Cloud top 52 sessions ago
		cloudTop52 := leadingSpanA52
		if leadingSpanB52 > leadingSpanA52 {
			cloudTop52 = leadingSpanB52
		}

		// ── Conditions ────────────────────────────────────────────────────────
		c1 := currentPrice > cloudTop
		c2 := currentPrice > priceFrom26
		c3 := convLine >= baseLine
		c4 := currentPrice > convLine
		c5 := currentPrice > cloudTop52

		// Condition 6 — cloud cross-out (intraday OR gap-open)
		intradayBreakout := todayOpen <= cloudTop && currentPrice > cloudTop
		gapOpenBreakout  := yesterdayClose <= cloudTop && todayOpen > cloudTop
		c6 := intradayBreakout || gapOpenBreakout

		if c1 && c2 && c3 && c4 && c5 && c6 {
			scenario := "INTRADAY"
			if gapOpenBreakout && !intradayBreakout {
				scenario = "GAP_OPEN"
			}
			sym := tokenToSymbol[token]
			results = append(results, Result{
				Symbol:    sym,
				Close:     currentPrice,
				Conv:      convLine,
				Base:      baseLine,
				CloudTop:  cloudTop,
				CloudBot:  cloudBot,
				TodayOpen: todayOpen,
				YestClose: yesterdayClose,
				Scenario:  scenario,
			})
		}
	}

	// ── 5. Print results ──────────────────────────────────────────────────────
	scanned := len(scanTokens) - skipped
	if *scanAll {
		fmt.Printf("[scanner] Scanned %d NSE tokens (%d skipped — insufficient history)\n", scanned, skipped)
	} else {
		fmt.Printf("[scanner] Scanned %d Nifty 500 tokens (%d skipped — insufficient history)\n", scanned, skipped)
	}
	fmt.Printf("[scanner] Found %d stocks satisfying all 6 Ichimoku conditions\n\n", len(results))

	if len(results) == 0 {
		fmt.Println("No stocks matched. Market may be closed or cloud cross happened in prior sessions.")
		return
	}

	// Sort by symbol for readability
	sortResults(results)

	// Header
	fmt.Printf("%-16s  %-10s  %-10s  %-10s  %-10s  %-10s  %-10s  %-10s  %s\n",
		"SYMBOL", "CLOSE", "CONV(9)", "BASE(26)", "CLOUD_TOP", "CLOUD_BOT", "TODAY_OPEN", "YEST_CLOSE", "SCENARIO")
	fmt.Println(strings.Repeat("─", 112))

	for _, r := range results {
		fmt.Printf("%-16s  %-10s  %-10s  %-10s  %-10s  %-10s  %-10s  %-10s  %s\n",
			r.Symbol,
			fmtPrice(r.Close),
			fmtPrice(r.Conv),
			fmtPrice(r.Base),
			fmtPrice(r.CloudTop),
			fmtPrice(r.CloudBot),
			fmtPrice(r.TodayOpen),
			fmtPrice(r.YestClose),
			r.Scenario,
		)
	}

	fmt.Printf("\n%d stock(s) satisfy the full Nifty 500 Ichimoku strategy\n", len(results))
}

// ── Helpers ───────────────────────────────────────────────────────────────────

type Candle struct {
	Date  time.Time
	Open  float64
	High  float64
	Low   float64
	Close float64
}

// loadAllTokens returns every distinct instrument token stored in daily_candles.
func loadAllTokens(db *sql.DB) (map[uint32]bool, error) {
	rows, err := db.Query(`SELECT DISTINCT token FROM daily_candles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tokens := make(map[uint32]bool, 5000)
	for rows.Next() {
		var tok uint32
		if err := rows.Scan(&tok); err != nil {
			continue
		}
		tokens[tok] = true
	}
	return tokens, rows.Err()
}

func loadCandles(db *sql.DB, token uint32) ([]Candle, error) {
	rows, err := db.Query(
		`SELECT date, open, high, low, close FROM daily_candles
		 WHERE token = ? ORDER BY date ASC`, token)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candles []Candle
	for rows.Next() {
		var c Candle
		var dateStr string
		if err := rows.Scan(&dateStr, &c.Open, &c.High, &c.Low, &c.Close); err != nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, dateStr)
		if err != nil {
			t, _ = time.Parse("2006-01-02", dateStr)
		}
		c.Date = t
		candles = append(candles, c)
	}
	return candles, rows.Err()
}

func periodMidpoint(candles []Candle, seedHigh, seedLow float64) float64 {
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

func fmtPrice(p float64) string {
	if p >= 1000 {
		return fmt.Sprintf("%.2f", p)
	}
	// Align decimals
	s := fmt.Sprintf("%.2f", math.Round(p*100)/100)
	return s
}

func sortResults(r []Result) {
	// Simple insertion sort by symbol
	for i := 1; i < len(r); i++ {
		key := r[i]
		j := i - 1
		for j >= 0 && r[j].Symbol > key.Symbol {
			r[j+1] = r[j]
			j--
		}
		r[j+1] = key
	}
}

// nseHTTPHeaders sets browser-like headers on req to satisfy NSE's Akamai CDN.
func nseHTTPHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Connection", "keep-alive")
}

// downloadNifty500Symbols downloads the NSE Nifty 500 CSV and returns a symbol set.
// NSE's Akamai CDN issues a cookie challenge: the homepage must be visited first
// to obtain session cookies before the CSV endpoint will respond with 200.
func downloadNifty500Symbols() (map[string]struct{}, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("cookie jar: %w", err)
	}
	client := &http.Client{Timeout: 20 * time.Second, Jar: jar}

	const maxAttempts = 3
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			log.Printf("[scanner] Retrying NSE CSV download (attempt %d/%d) after 3s...", attempt, maxAttempts)
			time.Sleep(3 * time.Second)
		}

		// Step 1: Prime session cookies via the NSE homepage.
		primer, err := http.NewRequest(http.MethodGet, "https://www.nseindia.com/", nil)
		if err != nil {
			lastErr = fmt.Errorf("primer request: %w", err)
			continue
		}
		nseHTTPHeaders(primer)
		pr, err := client.Do(primer)
		if err != nil {
			lastErr = fmt.Errorf("homepage primer: %w", err)
			continue
		}
		io.Copy(io.Discard, pr.Body) //nolint:errcheck
		pr.Body.Close()
		time.Sleep(1 * time.Second)

		// Step 2: Download the CSV with the session cookies now in the jar.
		req, err := http.NewRequest(http.MethodGet, nifty500URL, nil)
		if err != nil {
			lastErr = fmt.Errorf("CSV request: %w", err)
			continue
		}
		nseHTTPHeaders(req)
		req.Header.Set("Referer", "https://www.nseindia.com/market-data/live-equity-market")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("HTTP GET %s: %w", nifty500URL, err)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP GET %s: %s", nifty500URL, resp.Status)
			if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == 429 {
				continue
			}
			return nil, lastErr
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("read body: %w", err)
			continue
		}
		content := strings.TrimPrefix(string(body), "\xef\xbb\xbf")

		reader := csv.NewReader(strings.NewReader(content))
		reader.TrimLeadingSpace = true
		headers, err := reader.Read()
		if err != nil {
			return nil, err
		}
		symbolCol := -1
		for i, h := range headers {
			if strings.EqualFold(strings.TrimSpace(h), "Symbol") {
				symbolCol = i
				break
			}
		}
		if symbolCol < 0 {
			return nil, fmt.Errorf("no Symbol column in CSV; headers: %v", headers)
		}
		symbols := make(map[string]struct{}, 500)
		for {
			row, err := reader.Read()
			if err == io.EOF {
				break
			}
			if err != nil || symbolCol >= len(row) {
				continue
			}
			if sym := strings.TrimSpace(row[symbolCol]); sym != "" {
				symbols[sym] = struct{}{}
			}
		}
		if len(symbols) == 0 {
			lastErr = fmt.Errorf("CSV parsed but no symbols found")
			continue
		}
		return symbols, nil
	}
	return nil, lastErr
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if strings.TrimSpace(v) == "" {
		log.Fatalf("required env var %q not set", key)
	}
	return v
}

func getenvOr(key, fallback string) string {
	if v := os.Getenv(key); strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}
