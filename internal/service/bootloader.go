package service

import (
	"context"
	"log"
	"sync"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
	"golang.org/x/time/rate"

	"algotrader/internal/domain"
	"algotrader/internal/repository"
)

const (
	// bootWorkers is the number of goroutines used for parallel historical data fetching.
	// Zerodha allows 3 historical data API requests per second, so 3 workers with a shared
	// rate limiter at 3 req/s fully saturates the quota without exceeding it.
	bootWorkers = 3

	// zerodhaHistoricalRPS is Zerodha's documented rate limit for the historical data API.
	zerodhaHistoricalRPS = 3

	// progressInterval controls how often the bootloader logs a progress update.
	progressInterval = 50
)

// Bootloader orchestrates the pre-market RAM cache population sequence.
// It implements a smart two-phase strategy for each tracked instrument token:
//  1. Check SQLite first — if 3 years of data already exists locally, load it into RAM.
//  2. If data is absent (first-time boot or corrupt state), fall back to the Zerodha API,
//     persist the response to SQLite, then load it into RAM.
//
// On first run with full NSE (~1,900 tokens), the parallel boot completes in ~6 minutes.
// Subsequent boots are instant (SQLite fast-path for all tokens).
type Bootloader struct {
	db  *repository.DB
	ctx *domain.MarketContext
	kc  *kiteconnect.Client
}

// NewBootloader constructs a Bootloader wired to the database, RAM context, and Kite client.
func NewBootloader(db *repository.DB, ctx *domain.MarketContext, kc *kiteconnect.Client) *Bootloader {
	return &Bootloader{
		db:  db,
		ctx: ctx,
		kc:  kc,
	}
}

// Boot executes the full pre-market bootstrap sequence for the provided instrument tokens.
//
// It uses a pool of bootWorkers goroutines sharing a token-bucket rate limiter capped at
// zerodhaHistoricalRPS requests/second. Tokens already present in SQLite skip the API call
// entirely (consuming no rate-limit slots) and are loaded directly into the RAM cache.
//
// On completion, every token in the slice will have its 3-year historical candle data
// available in the MarketContext RAM cache for strategy workers to read.
func (b *Bootloader) Boot(tokens []uint32) {
	total := len(tokens)
	log.Printf("[bootloader] Starting parallel boot sequence for %d tokens (%d workers, %d req/s limit)",
		total, bootWorkers, zerodhaHistoricalRPS)

	// Shared rate limiter: burst of bootWorkers, steady-state at zerodhaHistoricalRPS/s.
	// Only API calls (slow path) consume a token; SQLite loads bypass the limiter.
	limiter := rate.NewLimiter(rate.Limit(zerodhaHistoricalRPS), bootWorkers)

	// Feed all tokens into a buffered work channel.
	work := make(chan uint32, total)
	for _, tok := range tokens {
		work <- tok
	}
	close(work)

	// Shared progress counter (atomic-ish via mutex is fine — logging is not hot path).
	var (
		mu        sync.Mutex
		completed int
	)

	var wg sync.WaitGroup
	for w := 0; w < bootWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for token := range work {
				b.processToken(workerID, token, limiter)

				mu.Lock()
				completed++
				if completed%progressInterval == 0 || completed == total {
					pct := completed * 100 / total
					log.Printf("[bootloader] Progress: [%d/%d] %d%% complete", completed, total, pct)
				}
				mu.Unlock()
			}
		}(w)
	}

	wg.Wait()
	log.Printf("[bootloader] Boot sequence complete. RAM cache contains data for %d tokens", b.ctx.TokenCount())
}

// processToken handles a single token: SQLite fast-path, no-data skip-list, or API slow-path.
func (b *Bootloader) processToken(workerID int, token uint32, limiter *rate.Limiter) {
	// Fast path 1: data already exists in SQLite — load into RAM, no API call.
	count, err := b.db.CountHistory(token)
	if err != nil {
		log.Printf("[bootloader] worker=%d ERROR counting history for token %d: %v — skipping", workerID, token, err)
		return
	}

	if count > 0 {
		candles, err := b.db.LoadHistory(token)
		if err != nil {
			log.Printf("[bootloader] worker=%d ERROR loading SQLite history for token %d: %v — skipping", workerID, token, err)
			return
		}

		// Incremental backfill: if the latest stored candle is more than one day
		// old, fetch the missing days from the API and persist them before loading
		// into the RAM cache. This keeps the DB current on every engine boot.
		if len(candles) > 0 {
			latest := candles[len(candles)-1].Date.Time
			yesterday := time.Now().AddDate(0, 0, -1)
			if latest.Before(yesterday) {
				if err := limiter.Wait(context.Background()); err == nil {
					fromDate := latest.Add(24 * time.Hour)
					toDate := time.Now()
					fresh, fetchErr := b.fetchFromAPI(token, fromDate, toDate)
					if fetchErr != nil {
						log.Printf("[bootloader] worker=%d WARN backfill fetch failed for token %d: %v — using stale data", workerID, token, fetchErr)
					} else if len(fresh) > 0 {
						if saveErr := b.db.SaveCandles(token, fresh); saveErr != nil {
							log.Printf("[bootloader] worker=%d WARN backfill save failed for token %d: %v", workerID, token, saveErr)
						}
						candles = append(candles, fresh...)
					}
				}
			}
		}

		b.ctx.Set(token, candles)
		return
	}

	// Fast path 2: token is in the no-data skip list — known zero-candle instrument.
	// Skip the API call entirely to avoid wasting rate-limit quota on delisted stocks.
	noData, err := b.db.IsNoDataToken(token)
	if err != nil {
		log.Printf("[bootloader] worker=%d ERROR checking no-data list for token %d: %v — will attempt API", workerID, token, err)
	} else if noData {
		return // silently skip — already known to have no data
	}

	// Slow path: no local data and not in skip list. Wait for a rate-limit slot.
	if err := limiter.Wait(context.Background()); err != nil {
		log.Printf("[bootloader] worker=%d rate limiter error for token %d: %v — skipping", workerID, token, err)
		return
	}

	toDate := time.Now()
	fromDate := toDate.AddDate(-3, 0, 0)

	candles, err := b.fetchFromAPI(token, fromDate, toDate)
	if err != nil {
		log.Printf("[bootloader] worker=%d ERROR fetching API data for token %d: %v — skipping", workerID, token, err)
		return
	}
	if len(candles) == 0 {
		// Permanently record this token in the skip list so future boots bypass the API call.
		if err := b.db.MarkNoData(token); err != nil {
			log.Printf("[bootloader] worker=%d ERROR marking no-data for token %d: %v", workerID, token, err)
		}
		return
	}

	if err := b.db.SaveCandles(token, candles); err != nil {
		log.Printf("[bootloader] worker=%d ERROR saving candles for token %d: %v — continuing with RAM-only cache", workerID, token, err)
	}
	b.ctx.Set(token, candles)
}

// EoDRollup performs the end-of-day data extension for all tracked tokens.
// It fetches today's now-closed daily candle from the Zerodha API,
// appends it to both SQLite and the RAM cache, then runs the 3-year purge.
//
// This is called by the scheduler at 15:45 on weekdays, after market close.
func (b *Bootloader) EoDRollup(tokens []uint32) {
	log.Printf("[bootloader] Starting EoD rollup for %d tokens", len(tokens))

	limiter := rate.NewLimiter(rate.Limit(zerodhaHistoricalRPS), bootWorkers)

	ist, istErr := time.LoadLocation("Asia/Kolkata")
	if istErr != nil {
		ist = time.FixedZone("IST", 5*3600+30*60)
	}
	today := time.Now().In(ist)
	fromDate := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, ist)
	toDate := today

	work := make(chan uint32, len(tokens))
	for _, tok := range tokens {
		work <- tok
	}
	close(work)

	var wg sync.WaitGroup
	for w := 0; w < bootWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for token := range work {
				// Skip tokens permanently flagged as no-data — same guard as Boot().
				// Avoids burning a rate-limit slot on delisted/suspended instruments.
				noData, ndErr := b.db.IsNoDataToken(token)
				if ndErr != nil {
					log.Printf("[bootloader] EoD worker=%d ERROR checking no-data list for token %d: %v — will attempt API", workerID, token, ndErr)
				} else if noData {
					continue
				}

				if err := limiter.Wait(context.Background()); err != nil {
					log.Printf("[bootloader] EoD worker=%d rate limiter error for token %d: %v", workerID, token, err)
					continue
				}

				candles, err := b.fetchFromAPI(token, fromDate, toDate)
				if err != nil {
					log.Printf("[bootloader] EoD worker=%d ERROR for token %d: %v — skipping", workerID, token, err)
					continue
				}
				if len(candles) == 0 {
					continue
				}

				if err := b.db.SaveCandles(token, candles); err != nil {
					log.Printf("[bootloader] EoD worker=%d ERROR saving token %d: %v", workerID, token, err)
				}
				for _, c := range candles {
					b.ctx.Append(token, c)
				}
			}
		}(w)
	}

	wg.Wait()

	if err := b.db.DropOldData(); err != nil {
		log.Printf("[bootloader] EoD ERROR during DropOldData purge: %v", err)
	}
	log.Println("[bootloader] EoD rollup complete")
}

// fetchFromAPI calls the Zerodha Kite Connect historical data endpoint for the given
// instrument token and date range, requesting daily ("day") interval candles.
func (b *Bootloader) fetchFromAPI(token uint32, from, to time.Time) ([]kiteconnect.HistoricalData, error) {
	return b.kc.GetHistoricalData(int(token), "day", from, to, false, false)
}
