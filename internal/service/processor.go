package service

import (
	"log"
	"runtime/debug"
	"sync"
	"time"

	"github.com/zerodha/gokiteconnect/v4/models"

	"algotrader/internal/domain"
	"algotrader/internal/strategy"
	"algotrader/internal/transport/discord"
)

// Processor is the concurrent tick evaluation engine.
// It manages a fixed-size worker goroutine pool that drains a high-throughput tick channel,
// applies registered strategies to each tick, and routes confirmed signals to Discord.
//
// Architecture:
//   - A single buffered input channel (chan models.Tick, cap 50,000) receives ticks from
//     the Kite WebSocket feed.
//   - A configurable pool of worker goroutines (default: 50) concurrently drain the channel.
//   - A StateRegistry prevents duplicate signals for the same instrument across workers.
//   - Each registered Strategy is evaluated per tick; confirmed signals are sent to Discord.
type Processor struct {
	tickCh     chan models.Tick
	strategies []strategy.Strategy
	registry   *domain.StateRegistry
	mktCtx     *domain.MarketContext
	discord    discord.SignalSender // accepts *discord.Client or *discord.BotClient
	workers    int
	wg         sync.WaitGroup
	// symbolMap provides token → trading symbol resolution for alert formatting.
	// Written once at construction; safe for concurrent reads without a lock.
	symbolMap map[uint32]string
}

// NewProcessor constructs a Processor with the given tick channel capacity and worker pool size.
// Strategies can be registered after construction via RegisterStrategy.
func NewProcessor(
	bufferCapacity int,
	workerCount int,
	registry *domain.StateRegistry,
	mktCtx *domain.MarketContext,
	discordClient discord.SignalSender,
	symbolMap map[uint32]string,
) *Processor {
	if symbolMap == nil {
		symbolMap = make(map[uint32]string)
	}
	return &Processor{
		tickCh:    make(chan models.Tick, bufferCapacity),
		registry:  registry,
		mktCtx:    mktCtx,
		discord:   discordClient,
		workers:   workerCount,
		symbolMap: symbolMap,
	}
}

// RegisterStrategy appends a strategy to the evaluation pipeline.
// Strategies are evaluated in registration order on every tick.
// This method must be called before Start(); calling it after Start() is a data race.
func (p *Processor) RegisterStrategy(s strategy.Strategy) {
	p.strategies = append(p.strategies, s)
	log.Printf("[processor] Registered strategy: %s", s.Name())
}

// Start launches the worker goroutine pool. Each worker independently drains the tick
// channel and evaluates all registered strategies. Workers run until the tick channel
// is closed, at which point they exit cleanly and the WaitGroup is decremented.
func (p *Processor) Start() {
	log.Printf("[processor] Starting %d worker goroutines (tick channel capacity: %d)", p.workers, cap(p.tickCh))
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go p.runWorker(i)
	}
}

// Stop closes the tick channel, signaling all workers to drain remaining ticks and exit.
// It blocks until every worker goroutine has returned, ensuring clean shutdown.
func (p *Processor) Stop() {
	log.Println("[processor] Stopping: closing tick channel and waiting for workers to drain")
	close(p.tickCh)
	p.wg.Wait()
	log.Println("[processor] All workers exited cleanly")
}

// Enqueue submits a tick to the processing channel in a non-blocking fashion.
// If the channel is at capacity (back-pressure scenario), the tick is dropped and
// a warning is logged to prevent the WebSocket feed goroutine from stalling.
func (p *Processor) Enqueue(tick models.Tick) {
	select {
	case p.tickCh <- tick:
	default:
		log.Printf("[processor] WARN: tick channel full (cap=%d), dropping tick for token %d",
			cap(p.tickCh), tick.InstrumentToken)
	}
}

// TickChannel exposes the write end of the tick channel so external feed adapters
// (e.g., the Kite WebSocket OnTick callback) can submit ticks directly.
func (p *Processor) TickChannel() chan<- models.Tick {
	return p.tickCh
}

// runWorker is the per-goroutine event loop. It drains the tick channel until it is
// closed, evaluating all registered strategies for each received tick.
func (p *Processor) runWorker(id int) {
	defer p.wg.Done()
	log.Printf("[processor] Worker %d started", id)

	for tick := range p.tickCh {
		p.processTick(tick)
	}

	log.Printf("[processor] Worker %d exiting — tick channel closed", id)
}

// processTick evaluates all registered strategies for the given tick.
// It first updates the IntradayCache with the latest price so that strategies
// always see a current today-OHLC snapshot via ctx.Intraday.
// It enforces the StateRegistry lock to ensure only the first worker to detect a
// breakout for a given instrument token executes the alert pipeline.
func (p *Processor) processTick(tick models.Tick) {
	// Always use wall-clock time for the intraday day-boundary check.
	// tick.LastTradeTime carries the timestamp of the last actual trade — for stale
	// after-hours Zerodha packets this is yesterday's date, which would cause the
	// intraday cache to wrongly report marketOpen=true on a new calendar day.
	// Wall-clock time is always correct for "what day are we on right now".
	p.mktCtx.Intraday.UpdateAndGet(tick.InstrumentToken, tick.LastPrice, time.Now())

	for _, strat := range p.strategies {
		// Evaluate the strategy inside a recover so a panic in any strategy
		// implementation does not kill the worker goroutine permanently.
		var fired bool
		var signal domain.OrderSignal
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[processor] PANIC in strategy %s for token %d: %v\n%s",
						strat.Name(), tick.InstrumentToken, r, debug.Stack())
				}
			}()
			fired, signal = strat.Evaluate(tick, p.mktCtx)
		}()
		if !fired {
			continue
		}

		// Resolve TradingSymbol from the lookup map so Discord alerts show the
		// instrument name (e.g. "RELIANCE") instead of an empty string.
		if signal.TradingSymbol == "" {
			if sym, ok := p.symbolMap[tick.InstrumentToken]; ok {
				signal.TradingSymbol = sym
			} else {
				signal.TradingSymbol = "UNKNOWN"
			}
		}

		// Acquire this strategy's daily alert slot for the token. Scoping by
		// strategy name keeps a high-frequency strategy from consuming the slot
		// and silently suppressing a selective one on the same stock.
		if !p.registry.TryLock(strat.Name(), tick.InstrumentToken) {
			log.Printf("[processor] Duplicate signal suppressed for token %d (strategy: %s)",
				tick.InstrumentToken, strat.Name())
			continue
		}

		// We hold the lock — execute the alert pipeline asynchronously so we don't
		// block the worker from processing subsequent ticks.
		go p.dispatchSignal(signal)
	}
}

// dispatchSignal sends the OrderSignal notification via Discord.
// Since StateRegistry now uses daily locks, we NO LONGER release the lock here.
// This guarantees we only ever send ONE alert per stock per day.
// It runs in its own goroutine to avoid blocking the worker pool.
func (p *Processor) dispatchSignal(signal domain.OrderSignal) {
	log.Printf("[processor] Signal fired: %s", signal.String())

	if err := p.discord.SendSignal(signal); err != nil {
		log.Printf("[processor] ERROR sending Discord alert for token %d (strategy: %s): %v",
			signal.InstrumentToken, signal.StrategyName, err)
		return
	}
	log.Printf("[processor] Discord alert delivered for token %d", signal.InstrumentToken)
}
