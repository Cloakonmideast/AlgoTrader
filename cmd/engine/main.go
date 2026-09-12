package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"algotrader/internal/config"
	"algotrader/internal/domain"
	"algotrader/internal/logger"
	"algotrader/internal/repository"
	"algotrader/internal/scheduler"
	"algotrader/internal/service"
	"algotrader/internal/strategy"
	"algotrader/internal/transport/discord"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
)

func main() {
	// ── Logging Setup ─────────────────────────────────────────────────────────
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile)
	log.SetOutput(os.Stdout)
	printBanner()

	// Initialise structured JSON logging. After this call every log.Printf line
	// is written to stdout (plain text) AND to logs/algotrader.log (JSON lines).
	if err := logger.Init("logs/algotrader.log"); err != nil {
		log.Printf("[main] WARNING: could not initialise JSON log file: %v", err)
	}

	// ── 1. Load Configuration (fail-fast on missing env vars) ─────────────────
	log.Println("[main] Loading configuration from environment variables")
	cfg := config.Load()

	if cfg.ScanNifty500 {
		log.Println("[main] Mode: NIFTY 500 SCAN — constituents will be fetched from NSE India CSV")
	} else if cfg.ScanFullNSE {
		log.Println("[main] Mode: FULL NSE SCAN — instrument tokens will be auto-discovered")
	} else {
		log.Printf("[main] Mode: MANUAL — tracking %d instrument tokens", len(cfg.InstrumentTokens))
	}

	// ── 2. Initialize SQLite Database ─────────────────────────────────────────
	log.Printf("[main] Opening SQLite database: %s", cfg.DatabasePath)
	db, err := repository.NewDB(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("[main] FATAL: failed to initialize database: %v", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			log.Printf("[main] WARNING: error closing database during shutdown: %v", closeErr)
		}
	}()

	// ── 3. Initialize In-Process RAM Cache ────────────────────────────────────
	log.Println("[main] Initializing MarketContext RAM cache")
	mktCtx := domain.NewMarketContext()

	// ── 4. Initialize Zerodha Kite Connect REST Client ────────────────────────
	log.Println("[main] Initializing Zerodha Kite Connect API client")
	kc := kiteconnect.New(cfg.KiteAPIKey)
	kc.SetAccessToken(cfg.KiteAccessToken)

	// ── 5. Resolve Instrument Token List ──────────────────────────────────────
	var tokens []uint32
	if cfg.ScanNifty500 {
		log.Println("[main] Discovering Nifty 500 instruments...")
		tokens, err = service.FetchNifty500Tokens(kc)
		if err != nil {
			log.Fatalf("[main] FATAL: Nifty 500 instrument discovery failed: %v", err)
		}
		log.Printf("[main] Discovered %d Nifty 500 instruments", len(tokens))
	} else if cfg.ScanFullNSE {
		log.Println("[main] Discovering all NSE EQ instruments via Zerodha API...")
		tokens, err = service.FetchNSEEquityTokens(kc)
		if err != nil {
			log.Fatalf("[main] FATAL: instrument discovery failed: %v", err)
		}
		log.Printf("[main] Discovered %d NSE EQ instruments", len(tokens))
	} else {
		tokens = cfg.InstrumentTokens
	}

	// ── 5b. Build Token → Symbol Lookup Map ───────────────────────────────────
	// The Zerodha WebSocket binary packet does not carry the trading symbol;
	// we resolve it here from the instrument master so alerts show real names.
	log.Println("[main] Building token-to-symbol lookup map from Zerodha instrument master")
	symbolMap := make(map[uint32]string, len(tokens))
	if instruments, instErr := kc.GetInstrumentsByExchange("NSE"); instErr != nil {
		log.Printf("[main] WARNING: could not fetch instrument master for symbol resolution: %v", instErr)
	} else {
		for _, inst := range instruments {
			symbolMap[uint32(inst.InstrumentToken)] = inst.Tradingsymbol
		}
		log.Printf("[main] Symbol map built: %d entries", len(symbolMap))
	}

	// ── 6. Initialize Discord Webhook Client (system / lifecycle alerts) ──────
	log.Println("[main] Initializing Discord webhook transport client")
	discordClient := discord.NewClient(cfg.DiscordWebhookURL)

	// ── 7. Initialize Domain StateRegistry ────────────────────────────────────
	log.Println("[main] Initializing StateRegistry for concurrent signal deduplication")
	registry := domain.NewStateRegistry()

	// ── 8. Build Tick Processor ───────────────────────────────────────────────
	log.Printf("[main] Building tick processor: %d workers, channel capacity %d",
		cfg.WorkerPoolSize, cfg.TickChannelCapacity)
	proc := service.NewProcessor(
		cfg.TickChannelCapacity,
		cfg.WorkerPoolSize,
		registry,
		mktCtx,
		discordClient, // webhook: sends stock+strategy name alerts
		symbolMap,
	)

	// Register strategies — add more implementations here as needed.
	proc.RegisterStrategy(strategy.NewIchimokuKinkoHyo())

	// RSI now emits only the two GFS setups, both requiring monthly RSI > 60 and
	// weekly RSI > 60. They differ in where the daily leg turns up:
	//
	//   RSI (GFS)          — yesterday's daily RSI in 37–44, today higher than
	//                        yesterday and above 40. A deep pullback turning at
	//                        the bear/bull boundary.
	//   RSI (Advanced GFS) — yesterday's daily RSI in 57–63, today higher than
	//                        yesterday and above 60. A shallow pause that never
	//                        leaves bullish territory.
	//
	// The allowlist is now redundant — the strategy emits nothing else — but it
	// is kept as a standing guarantee that only these two ever reach Discord, so
	// adding a signal later cannot silently start alerting.
	//
	// Either signal can repeat on consecutive sessions; the StateRegistry caps
	// delivery at one RSI alert per stock per day.
	proc.RegisterStrategy(strategy.OnlySignals(
		strategy.NewRSI(14),
		"RSI (GFS)",
		"RSI (Advanced GFS)",
	))

	// ── 9. Initialize Bootloader ──────────────────────────────────────────────
	log.Println("[main] Initializing pre-market bootloader")
	bootloader := service.NewBootloader(db, mktCtx, kc)

	// ── 10. Pre-market Boot Sequence ──────────────────────────────────────────
	// Populates the RAM cache from SQLite (fast path) or Zerodha API (first-run path).
	// With full NSE mode, this runs 3 workers in parallel at Zerodha's 3 req/s limit.
	log.Printf("[main] Executing pre-market boot sequence for %d tokens", len(tokens))
	bootloader.Boot(tokens)
	log.Printf("[main] Boot complete: RAM cache has data for %d tokens", mktCtx.TokenCount())

	// ── 10.5 Purge Delisted Instruments from Database ─────────────────────────
	// Remove candle rows for any token no longer present in the live NSE EQ list.
	// This keeps the database clean as stocks get delisted or suspended over time.
	if cfg.ScanFullNSE || cfg.ScanNifty500 {
		log.Println("[main] Running delisted instrument cleanup...")
		deleted, err := db.CleanupDelisted(tokens)
		if err != nil {
			log.Printf("[main] WARNING: CleanupDelisted failed: %v", err)
		} else {
			log.Printf("[main] Cleanup complete: removed %d candle rows for delisted instruments", deleted)
		}
	}

	// ── 11. Initialize & Start Market Scheduler ───────────────────────────────
	log.Println("[main] Initializing market lifecycle scheduler")
	sched := scheduler.NewMarketScheduler(bootloader, tokens)
	sched.Start()

	// ── 12. Start Tick Processor Worker Pool ──────────────────────────────────
	log.Println("[main] Starting tick processor worker pool")
	proc.Start()

	// ── 13. Initialize & Wire 3 WebSocket Tickers ─────────────────────────────
	// MultiTicker distributes all tokens across 3 independent Zerodha WebSocket
	// connections (Zerodha's per-API-key limit), each carrying ~1/3 of all tokens.
	log.Printf("[main] Initializing MultiTicker: 3 WebSocket connections for %d tokens", len(tokens))
	mt := service.NewMultiTicker(
		cfg.KiteAPIKey,
		cfg.KiteAccessToken,
		tokens,
		proc.Enqueue,
		discordClient,
	)
	mt.Start()

	// Send a single consolidated Discord alert after all WS connections initialise.
	_ = discordClient.SendAlert(
		"AlgoTrader Online",
		fmt.Sprintf("Engine started. Monitoring %d instruments across 3 WebSocket connections.", len(tokens)),
		false,
	)

	// ── 14. Block on OS Signals — Graceful Shutdown ───────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	sig := <-quit
	log.Printf("[main] Received OS signal: %s — initiating graceful shutdown", sig)
	_ = discordClient.SendAlert(
		"AlgoTrader Shutting Down",
		fmt.Sprintf("Signal received: %s. Graceful shutdown initiated.", sig),
		true,
	)

	// Shutdown sequence (reverse of initialisation order):

	// 1. Stop all 3 WebSocket connections to prevent new ticks from arriving.
	log.Println("[main] Step 1/4: Closing MultiTicker (3 WebSocket connections)")
	mt.Stop()

	// 2. Stop the cron scheduler (waits for any in-progress job to finish).
	log.Println("[main] Step 2/4: Stopping market lifecycle scheduler")
	sched.Stop()

	// 3. Stop the processor (closes channel, drains remaining ticks, waits for workers).
	log.Println("[main] Step 3/4: Stopping tick processor worker pool")
	proc.Stop()

	// 4. Database is closed by the deferred db.Close() at function return.
	log.Println("[main] Step 4/4: Flushing database via deferred close handler")

	log.Println("[main] AlgoTrader engine shut down cleanly. Goodbye.")

	// Flush and close the JSON log file last — after all goroutines have stopped writing.
	if err := logger.Close(); err != nil {
		log.Printf("[main] WARNING: error closing log file: %v", err)
	}
}

// printBanner outputs the application startup header.
func printBanner() {
	fmt.Print(`
 ┌─────────────────────────────────────────────────┐
 │          A L G O T R A D E R   ENGINE           │
 │   Zerodha Kite Connect  •  SQLite  •  Discord   │
 │   Low-Latency Concurrent Algorithmic Trading    │
 └─────────────────────────────────────────────────┘

`)
}
