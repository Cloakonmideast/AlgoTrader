package service

import (
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
)

// NSE India Nifty 500 index constituent CSV URL.
// NSE publishes and updates this file whenever index composition changes.
const (
	nifty500URL = "https://archives.nseindia.com/content/indices/ind_nifty500list.csv"
)

// FetchNSEEquityTokens retrieves the complete NSE instrument dump from the Zerodha
// Kite Connect API, filters for equity (EQ) instruments only, and returns their
// instrument tokens as a []uint32 slice ready for use by the bootloader and tickers.
//
// The instrument dump is a CSV-backed list of all tradeable instruments. For NSE,
// this typically contains ~4,000 entries including futures, options, and indices.
// After filtering for instrument_type == "EQ", the result is ~9,700+ stocks including
// suspended, illiquid, and newly listed instruments with no trading activity.
func FetchNSEEquityTokens(kc *kiteconnect.Client) ([]uint32, error) {
	log.Println("[discovery] Fetching NSE instrument list from Zerodha API...")

	instruments, err := kc.GetInstrumentsByExchange("NSE")
	if err != nil {
		return nil, fmt.Errorf("discovery: GetInstruments(NSE): %w", err)
	}

	tokens := make([]uint32, 0, 2000)
	for _, inst := range instruments {
		// Filter: mainboard NSE equity only.
		// segment == "NSE" excludes SME (NSE_SME), NSEIFSC, etc.
		// instrument_type == "EQ" excludes futures, options, warrants, ETFs (ETF), indices (INDICES).
		if inst.InstrumentType == "EQ" && inst.Segment == "NSE" {
			tokens = append(tokens, uint32(inst.InstrumentToken))
		}
	}

	if len(tokens) == 0 {
		return nil, fmt.Errorf("discovery: zero EQ instruments returned for NSE — check API credentials")
	}

	log.Printf("[discovery] Found %d NSE EQ instruments", len(tokens))
	return tokens, nil
}

// FetchActiveNSETokens retrieves only NSE EQ instruments that are actively traded,
// identified by last_price > 0 in Zerodha's instrument master.
//
// Zerodha populates last_price in the instrument dump with the previous session's
// closing price. A value of 0 reliably indicates that the instrument is suspended,
// circuit-locked with no trades, freshly listed without a closing price, or effectively
// delisted. Filtering these out yields the practical universe of stocks that actually
// generate ticks during market hours — typically ~2,000–3,000 instruments.
func FetchActiveNSETokens(kc *kiteconnect.Client) ([]uint32, error) {
	log.Println("[discovery] Fetching NSE instrument list from Zerodha API...")

	instruments, err := kc.GetInstrumentsByExchange("NSE")
	if err != nil {
		return nil, fmt.Errorf("discovery: GetInstruments(NSE): %w", err)
	}

	var total, filtered int
	tokens := make([]uint32, 0, 3000)
	for _, inst := range instruments {
		if inst.InstrumentType != "EQ" || inst.Segment != "NSE" {
			continue
		}
		total++
		if inst.LastPrice > 0 {
			tokens = append(tokens, uint32(inst.InstrumentToken))
			filtered++
		}
	}

	if len(tokens) == 0 {
		return nil, fmt.Errorf("discovery: zero active instruments found — check API credentials")
	}

	log.Printf("[discovery] Active NSE EQ filter: %d/%d instruments have last_price > 0", filtered, total)
	return tokens, nil
}

// FetchNifty500Tokens returns instrument tokens for only the Nifty 500 constituents.
//
// It works in two steps:
//  1. Attempts to download the official Nifty 500 CSV from NSE India. If that
//     fails (e.g. NSE's Akamai CDN blocks the request), it falls back to the
//     embedded static symbol list (see nifty500_symbols_embed.go) which is
//     regenerated periodically with: go run ./scripts/gen_nifty500_embed.go
//  2. Fetches the full Zerodha NSE instrument master and cross-references it
//     against those symbols, keeping only EQ instruments present in the list.
//
// This is dramatically narrower than FetchNSEEquityTokens (~500 vs ~9,700 tokens),
// cutting first-run boot time and Zerodha WebSocket subscription slot usage significantly.
func FetchNifty500Tokens(kc *kiteconnect.Client) ([]uint32, error) {
	log.Println("[discovery] Fetching Nifty 500 constituent list from NSE India...")

	nifty500Symbols, err := downloadNifty500Symbols()
	if err != nil {
		// NSE's CDN (Akamai) frequently blocks programmatic access.
		// Fall back to the embedded static list rather than crashing the engine.
		log.Printf("[discovery] WARNING: live NSE CSV download failed (%v); using embedded static Nifty 500 list", err)
		nifty500Symbols = embeddedNifty500SymbolSet()
		log.Printf("[discovery] Embedded static list loaded: %d symbols", len(nifty500Symbols))
	} else {
		log.Printf("[discovery] Nifty 500 CSV parsed: %d symbols found", len(nifty500Symbols))
	}

	log.Println("[discovery] Fetching NSE instrument master from Zerodha API...")
	instruments, err := kc.GetInstrumentsByExchange("NSE")
	if err != nil {
		return nil, fmt.Errorf("discovery: GetInstruments(NSE): %w", err)
	}

	tokens := make([]uint32, 0, 500)
	matched := 0
	for _, inst := range instruments {
		if inst.InstrumentType != "EQ" || inst.Segment != "NSE" {
			continue
		}
		if _, ok := nifty500Symbols[inst.Tradingsymbol]; ok {
			tokens = append(tokens, uint32(inst.InstrumentToken))
			matched++
		}
	}

	if matched == 0 {
		return nil, fmt.Errorf("discovery: zero Nifty 500 instruments matched — check API credentials or NSE CSV format")
	}

	log.Printf("[discovery] Nifty 500 cross-reference complete: %d/%d symbols matched to Zerodha tokens",
		matched, len(nifty500Symbols))
	return tokens, nil
}

// EmbeddedNifty500Symbols exposes the static embedded Nifty 500 constituent list
// as a lookup set. It requires no network access and no Kite credentials, which
// lets offline tools (the backtester) filter a universe by index membership.
func EmbeddedNifty500Symbols() map[string]struct{} {
	return embeddedNifty500SymbolSet()
}

// embeddedNifty500SymbolSet converts the static embedded symbol slice into a
// map[string]struct{} for O(1) lookup, identical in shape to the live CSV result.
func embeddedNifty500SymbolSet() map[string]struct{} {
	m := make(map[string]struct{}, len(nifty500EmbeddedSymbols))
	for _, sym := range nifty500EmbeddedSymbols {
		m[sym] = struct{}{}
	}
	return m
}


// nseHeaders sets the browser-like headers that NSE's Akamai CDN requires.
// Called on every outbound request to both the homepage primer and the CSV URL.
func nseHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Connection", "keep-alive")
}

// downloadNifty500Symbols fetches the NSE Nifty 500 CSV and returns a set of
// trading symbols (e.g. "RELIANCE", "INFY") as map keys for O(1) lookup.
//
// NSE India's Akamai CDN enforces a cookie challenge: a direct request to the
// CSV URL returns 503 until the client has first visited nseindia.com to receive
// session cookies (nsit, nseappid, etc.). We use a shared cookiejar so those
// cookies are automatically included in the follow-up CSV download.
// Up to 3 attempts are made with 3-second backoff to handle transient failures.
func downloadNifty500Symbols() (map[string]struct{}, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create cookie jar: %w", err)
	}
	client := &http.Client{
		Timeout: 20 * time.Second,
		Jar:     jar,
	}

	const maxAttempts = 3
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			log.Printf("[discovery] Retrying NSE CSV download (attempt %d/%d) after 3s...", attempt, maxAttempts)
			time.Sleep(3 * time.Second)
		}

		// Step 1: Prime the cookie jar by visiting the NSE homepage.
		// NSE's CDN (Akamai) sets mandatory session cookies (nsit, nseappid, etc.)
		// here; without them the CSV endpoint returns 503.
		log.Printf("[discovery] Priming NSE session cookies (attempt %d/%d)...", attempt, maxAttempts)
		primer, err := http.NewRequest(http.MethodGet, "https://www.nseindia.com/", nil)
		if err != nil {
			lastErr = fmt.Errorf("build primer request: %w", err)
			continue
		}
		nseHeaders(primer)
		primerResp, err := client.Do(primer)
		if err != nil {
			lastErr = fmt.Errorf("homepage primer GET: %w", err)
			continue
		}
		// Drain the body so the connection can be reused, then close.
		io.Copy(io.Discard, primerResp.Body) //nolint:errcheck
		primerResp.Body.Close()
		log.Printf("[discovery] NSE homepage primer: HTTP %s", primerResp.Status)

		// Brief pause so Akamai's challenge timing is satisfied.
		time.Sleep(1 * time.Second)

		// Step 2: Download the CSV — the cookie jar now carries the session cookies.
		req, err := http.NewRequest(http.MethodGet, nifty500URL, nil)
		if err != nil {
			lastErr = fmt.Errorf("build CSV request: %w", err)
			continue
		}
		nseHeaders(req)
		req.Header.Set("Referer", "https://www.nseindia.com/market-data/live-equity-market")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("HTTP GET %s: %w", nifty500URL, err)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP GET %s: unexpected status %s", nifty500URL, resp.Status)
			// 503 / 429 are transient — retry. Other errors are fatal.
			if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == 429 {
				continue
			}
			return nil, lastErr
		}

		symbols, err := parseNifty500CSV(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		return symbols, nil
	}

	return nil, lastErr
}

// parseNifty500CSV parses an NSE Nifty 500 CSV and extracts the "Symbol" column.
// The NSE CSV may contain a BOM and/or extra whitespace which is handled here.
func parseNifty500CSV(r io.Reader) (map[string]struct{}, error) {
	// Read all bytes to handle the BOM if present.
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	// Strip UTF-8 BOM (EF BB BF) that NSE occasionally includes.
	content := strings.TrimPrefix(string(body), "\xef\xbb\xbf")

	reader := csv.NewReader(strings.NewReader(content))
	reader.TrimLeadingSpace = true

	headers, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read CSV header: %w", err)
	}

	// Locate the "Symbol" column index (case-insensitive, trim whitespace).
	symbolCol := -1
	for i, h := range headers {
		if strings.EqualFold(strings.TrimSpace(h), "Symbol") {
			symbolCol = i
			break
		}
	}
	if symbolCol == -1 {
		return nil, fmt.Errorf("CSV header does not contain a 'Symbol' column; got: %v", headers)
	}

	symbols := make(map[string]struct{}, 500)
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read CSV row: %w", err)
		}
		if symbolCol >= len(row) {
			continue
		}
		sym := strings.TrimSpace(row[symbolCol])
		if sym != "" {
			symbols[sym] = struct{}{}
		}
	}

	if len(symbols) == 0 {
		return nil, fmt.Errorf("CSV parsed but no symbols found — unexpected file format")
	}
	return symbols, nil
}

