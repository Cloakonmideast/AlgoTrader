// cmd/backfill/main.go
//
// Incremental DB backfill — fetches only the missing daily candles
// (from the last stored date up to today) for every token in the DB.
//
// Usage:
//
//	go run ./cmd/backfill
//
// Uses the same env vars as the main engine:
//
//	KITE_API_KEY, KITE_ACCESS_TOKEN, DATABASE_PATH
//
// Rate limit: 3 req/s (Zerodha historical API quota).
// With ~4,465 tokens and a ~9-day gap, expect ~25 minutes to complete.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	kiteconnect "github.com/zerodha/gokiteconnect/v4"
	"golang.org/x/time/rate"

	"algotrader/internal/repository"
)

const (
	workers       = 3
	ratePerSecond = 3

	// maxDaysPerRequest is Zerodha's ceiling for the "day" interval. A deeper
	// span must be split into several requests.
	maxDaysPerRequest = 1900
)

func main() {
	log.SetFlags(log.Ltime | log.Lshortfile)

	years := flag.Int("years", 0, "Deep backfill: fetch this many years of history for every token (0 = incremental gap fill only)")
	force := flag.Bool("force", false, "Deep backfill: refetch tokens that already reach back far enough")
	flag.Parse()

	if *years > 0 {
		fmt.Printf(`
╔══════════════════════════════════════════════════════════╗
║            ALGOTRADER — Deep History Backfill            ║
║   Fetching %2d years of daily candles for every token     ║
╚══════════════════════════════════════════════════════════╝`, *years)
	} else {
		fmt.Println(`
╔══════════════════════════════════════════════════════════╗
║         ALGOTRADER — Incremental DB Backfill             ║
║   Fetching missing candles from last date → today        ║
╚══════════════════════════════════════════════════════════╝`)
	}

	// ── Config ────────────────────────────────────────────────────────────────
	apiKey := mustEnv("KITE_API_KEY")
	accessToken := mustEnv("KITE_ACCESS_TOKEN")
	dbPath := getenvOr("DATABASE_PATH", "market_data.db")

	// ── Kite client ───────────────────────────────────────────────────────────
	kc := kiteconnect.New(apiKey)
	kc.SetAccessToken(accessToken)

	// ── Open DB ───────────────────────────────────────────────────────────────
	db, err := repository.NewDB(dbPath)
	if err != nil {
		log.Fatalf("cannot open db: %v", err)
	}
	defer db.Close()

	// ── Find the global latest date already in the DB ─────────────────────────
	rawDB, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?_journal_mode=WAL&_synchronous=NORMAL", dbPath))
	if err != nil {
		log.Fatalf("cannot open raw db: %v", err)
	}
	defer rawDB.Close()

	var latestStr string
	rawDB.QueryRow("SELECT MAX(date) FROM daily_candles").Scan(&latestStr)
	if latestStr == "" {
		log.Fatal("DB has no candles at all — run the engine first to do a full boot")
	}

	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		ist = time.FixedZone("IST", 5*3600+30*60)
	}

	if *years > 0 {
		deepBackfill(kc, db, rawDB, ist, *years, *force)
		return
	}

	lastDate, err := time.Parse(time.RFC3339, latestStr)
	if err != nil {
		lastDate, err = time.Parse("2006-01-02", latestStr)
		if err != nil {
			log.Fatalf("cannot parse latest date %q: %v", latestStr, err)
		}
	}

	// fromDate = day after the last stored candle (start of that IST day)
	fromDate := lastDate.Add(24 * time.Hour)
	fromDate = time.Date(fromDate.Year(), fromDate.Month(), fromDate.Day(), 0, 0, 0, 0, ist)

	// toDate = now (Kite API will only return closed candles anyway)
	toDate := time.Now().In(ist)

	if !fromDate.Before(toDate) {
		fmt.Printf("\n✅ DB is already up to date (latest: %s). Nothing to backfill.\n",
			lastDate.In(ist).Format("2006-01-02"))
		return
	}

	fmt.Printf("\n  Last stored date : %s\n", lastDate.In(ist).Format("2006-01-02"))
	fmt.Printf("  Fetching from    : %s\n", fromDate.Format("2006-01-02"))
	fmt.Printf("  Fetching to      : %s\n\n", toDate.Format("2006-01-02"))

	// ── Load all tokens from DB ───────────────────────────────────────────────
	rows, err := rawDB.Query("SELECT DISTINCT token FROM daily_candles ORDER BY token")
	if err != nil {
		log.Fatalf("cannot read tokens: %v", err)
	}
	var tokens []uint32
	for rows.Next() {
		var t uint32
		rows.Scan(&t)
		tokens = append(tokens, t)
	}
	rows.Close()
	log.Printf("[backfill] %d tokens to update", len(tokens))

	// ── Worker pool ───────────────────────────────────────────────────────────
	limiter := rate.NewLimiter(rate.Limit(ratePerSecond), workers)

	work := make(chan uint32, len(tokens))
	for _, t := range tokens {
		work <- t
	}
	close(work)

	var (
		mu       sync.Mutex
		done     int
		updated  int
		skipped  int
		errCount int
		total    = len(tokens)
	)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for token := range work {
				if err := limiter.Wait(context.Background()); err != nil {
					log.Printf("[backfill] worker=%d rate limiter error: %v", id, err)
					continue
				}

				candles, err := kc.GetHistoricalData(int(token), "day", fromDate, toDate, false, false)
				mu.Lock()
				done++
				pct := done * 100 / total
				if done%100 == 0 || done == total {
					log.Printf("[backfill] Progress [%d/%d] %d%% | updated=%d skipped=%d errors=%d",
						done, total, pct, updated, skipped, errCount)
				}
				mu.Unlock()

				if err != nil {
					mu.Lock()
					errCount++
					mu.Unlock()
					continue
				}
				if len(candles) == 0 {
					mu.Lock()
					skipped++
					mu.Unlock()
					continue
				}

				if err := db.SaveCandles(token, candles); err != nil {
					log.Printf("[backfill] worker=%d ERROR saving token %d: %v", id, token, err)
					mu.Lock()
					errCount++
					mu.Unlock()
					continue
				}
				mu.Lock()
				updated++
				mu.Unlock()
			}
		}(w)
	}

	wg.Wait()

	fmt.Printf(`
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  Backfill complete!
  Tokens updated  : %d
  Tokens skipped  : %d  (no new candles)
  Errors          : %d
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
`, updated, skipped, errCount)
}

// tokenSpan pairs a token with the oldest candle currently stored for it.
type tokenSpan struct {
	Token  uint32
	Oldest time.Time
}

// loadTokenSpans reads every token in the DB alongside its earliest stored date,
// which tells the deep backfill how far back each token still needs filling.
func loadTokenSpans(rawDB *sql.DB) ([]tokenSpan, error) {
	rows, err := rawDB.Query(`SELECT token, MIN(date) FROM daily_candles GROUP BY token ORDER BY token`)
	if err != nil {
		return nil, fmt.Errorf("read token spans: %w", err)
	}
	defer rows.Close()

	var spans []tokenSpan
	for rows.Next() {
		var (
			token   uint32
			dateStr string
		)
		if err := rows.Scan(&token, &dateStr); err != nil {
			return nil, fmt.Errorf("scan token span: %w", err)
		}
		t, err := time.Parse(time.RFC3339, dateStr)
		if err != nil {
			if t, err = time.Parse("2006-01-02", dateStr); err != nil {
				continue // unparseable — treat as absent and refetch the full span
			}
		}
		spans = append(spans, tokenSpan{Token: token, Oldest: t})
	}
	return spans, rows.Err()
}

// deepBackfill extends every token's history back `years` from today.
//
// Only the missing OLDER portion is fetched: a token already holding data from
// 2023 onward is asked for [target … 2023] rather than the whole span, which
// roughly halves the API calls. Zerodha caps a "day" request at 2000 sessions,
// so each token's span is split into maxDaysPerRequest chunks.
func deepBackfill(kc *kiteconnect.Client, db *repository.DB, rawDB *sql.DB, ist *time.Location, years int, force bool) {
	toDate := time.Now().In(ist)
	targetFrom := toDate.AddDate(-years, 0, 0)

	spans, err := loadTokenSpans(rawDB)
	if err != nil {
		log.Fatalf("[backfill] %v", err)
	}

	// A token whose oldest candle already sits at or before the target needs
	// nothing. The week of tolerance absorbs listings, holidays and weekends.
	tolerance := targetFrom.AddDate(0, 0, 7)
	var work []tokenSpan
	alreadyDeep := 0
	for _, s := range spans {
		if !force && !s.Oldest.After(tolerance) {
			alreadyDeep++
			continue
		}
		work = append(work, s)
	}

	fmt.Printf("\n  Target start   : %s\n", targetFrom.Format("2006-01-02"))
	fmt.Printf("  Fetching to    : %s\n", toDate.Format("2006-01-02"))
	fmt.Printf("  Tokens in DB   : %d\n", len(spans))
	fmt.Printf("  Already deep   : %d (skipped)\n", alreadyDeep)
	fmt.Printf("  To backfill    : %d\n", len(work))
	fmt.Printf("  Retention      : RETENTION_YEARS=%d — set this to >= %d or the EoD rollup will purge the new data\n\n",
		repository.RetentionYears(), years)

	if len(work) == 0 {
		fmt.Println("✅ Every token already reaches the target depth. Nothing to do.")
		return
	}

	limiter := rate.NewLimiter(rate.Limit(ratePerSecond), workers)
	jobs := make(chan tokenSpan, len(work))
	for _, s := range work {
		jobs <- s
	}
	close(jobs)

	var (
		mu       sync.Mutex
		done     int
		updated  int
		empty    int
		errCount int
		added    int
		total    = len(work)
	)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for span := range jobs {
				// Fetch only the gap below what we already hold.
				until := span.Oldest
				if force || until.After(toDate) {
					until = toDate
				}

				tokenAdded, failed := fetchRange(kc, db, limiter, span.Token, targetFrom, until)

				mu.Lock()
				done++
				added += tokenAdded
				switch {
				case failed:
					errCount++
				case tokenAdded > 0:
					updated++
				default:
					empty++
				}
				if done%25 == 0 || done == total {
					log.Printf("[backfill] Progress [%d/%d] %d%% | updated=%d empty=%d errors=%d candles=%d",
						done, total, done*100/total, updated, empty, errCount, added)
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	fmt.Printf(`
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  Deep backfill complete!
  Tokens updated  : %d
  Tokens empty    : %d  (no older data available)
  Errors          : %d
  Candles added   : %d
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

  Remember: export RETENTION_YEARS=%d before running the engine,
  or the 15:45 EoD rollup will purge everything past 3 years.
`, updated, empty, errCount, added, years)
}

// fetchRange pulls [from, to] for one token, splitting the span into chunks that
// respect Zerodha's per-request day ceiling, and persists each chunk as it lands.
// Partial success is kept: a later chunk failing does not discard earlier ones.
func fetchRange(kc *kiteconnect.Client, db *repository.DB, limiter *rate.Limiter, token uint32, from, to time.Time) (added int, failed bool) {
	for start := from; start.Before(to); {
		end := start.AddDate(0, 0, maxDaysPerRequest)
		if end.After(to) {
			end = to
		}

		if err := limiter.Wait(context.Background()); err != nil {
			log.Printf("[backfill] token %d rate limiter: %v", token, err)
			return added, true
		}

		candles, err := kc.GetHistoricalData(int(token), "day", start, end, false, false)
		if err != nil {
			log.Printf("[backfill] token %d [%s → %s]: %v",
				token, start.Format("2006-01-02"), end.Format("2006-01-02"), err)
			return added, true
		}
		if len(candles) > 0 {
			if err := db.SaveCandles(token, candles); err != nil {
				log.Printf("[backfill] token %d save failed: %v", token, err)
				return added, true
			}
			added += len(candles)
		}

		start = end.AddDate(0, 0, 1)
	}
	return added, false
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if strings.TrimSpace(v) == "" {
		log.Fatalf("required env var %q is not set", key)
	}
	return v
}

func getenvOr(key, fallback string) string {
	if v := os.Getenv(key); strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}
