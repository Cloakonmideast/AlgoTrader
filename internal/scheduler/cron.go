package scheduler

import (
	"log"
	"time"

	"github.com/robfig/cron/v3"

	"algotrader/internal/service"
)

// MarketScheduler wraps the embedded cron engine and wires it to the application's
// lifecycle services. It enforces weekday-only (Monday–Friday) execution of all jobs
// and uses the standard crontab schedule format supported by robfig/cron v3.
//
// Registered Jobs:
//  1. "45 8 * * 1-5"  → MorningBoot at 08:45 — maps DB matrices to RAM before market open.
//  2. "45 15 * * 1-5" → EoDRollup at 15:45 — fetches today's closed candle, appends to DB + RAM,
//     then executes the 3-year purge.
type MarketScheduler struct {
	cron       *cron.Cron
	bootloader *service.Bootloader
	tokens     []uint32
}

// NewMarketScheduler constructs the scheduler and registers all lifecycle cron jobs.
// The cron engine is configured with the seconds-precision parser disabled (standard 5-field crontab format).
func NewMarketScheduler(bootloader *service.Bootloader, tokens []uint32) *MarketScheduler {
	// Use standard 5-field minute-precision crontab format (no seconds field).
	// Pin to IST so cron times (08:45, 15:45) are Indian market times regardless
	// of the server's local timezone.
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		// Fallback: UTC offset +05:30 if the timezone database is unavailable.
		ist = time.FixedZone("IST", 5*3600+30*60)
	}
	c := cron.New(cron.WithLocation(ist))

	ms := &MarketScheduler{
		cron:       c,
		bootloader: bootloader,
		tokens:     tokens,
	}

	ms.registerJobs()
	return ms
}

// registerJobs installs all weekday cron schedules into the embedded cron engine.
// Both schedules use the "1-5" day-of-week field to restrict execution to Monday–Friday only.
func (ms *MarketScheduler) registerJobs() {
	// ── Job 1: Morning Boot (08:45 AM weekdays) ──────────────────────────────
	// Triggers before the Indian equity market opens (09:15 AM IST) to ensure the
	// RAM cache is fully populated and all strategies are ready to evaluate ticks.
	_, err := ms.cron.AddFunc("45 8 * * 1-5", func() {
		log.Println("[scheduler] 08:45 Morning Boot job triggered")
		ms.bootloader.Boot(ms.tokens)
		log.Println("[scheduler] 08:45 Morning Boot job complete")
	})
	if err != nil {
		log.Panicf("[scheduler] Failed to register MorningBoot cron job: %v", err)
	}

	// ── Job 2: End-of-Day Rollup (03:45 PM weekdays) ─────────────────────────
	// Triggers after market close (03:30 PM IST) to fetch today's newly closed daily candle,
	// persist it to SQLite, extend the RAM cache, and purge data older than 3 years.
	_, err = ms.cron.AddFunc("45 15 * * 1-5", func() {
		log.Println("[scheduler] 15:45 End-of-Day Rollup job triggered")
		ms.bootloader.EoDRollup(ms.tokens)
		log.Println("[scheduler] 15:45 End-of-Day Rollup job complete")
	})
	if err != nil {
		log.Panicf("[scheduler] Failed to register EoDRollup cron job: %v", err)
	}

	log.Println("[scheduler] Cron jobs registered: MorningBoot @ 08:45, EoDRollup @ 15:45 (Mon–Fri)")
}

// Start activates the cron engine. Jobs will execute automatically on their configured schedules.
// This is non-blocking; the cron engine runs its own internal goroutine.
func (ms *MarketScheduler) Start() {
	ms.cron.Start()
	log.Println("[scheduler] Cron engine started")
}

// Stop gracefully shuts down the cron engine, waiting for any currently executing job
// to complete before returning. This ensures no job is left mid-execution during shutdown.
func (ms *MarketScheduler) Stop() {
	log.Println("[scheduler] Stopping cron engine — waiting for active jobs to complete")
	ctx := ms.cron.Stop()
	<-ctx.Done()
	log.Println("[scheduler] Cron engine stopped cleanly")
}
