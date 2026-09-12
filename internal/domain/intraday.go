package domain

import (
	"sync"
	"time"
)

// IntradayCandle holds the running OHLC snapshot for a single instrument
// for the current trading day. It is built tick-by-tick from live WebSocket data.
type IntradayCandle struct {
	Open   float64
	High   float64
	Low    float64
	Close  float64   // last seen price — updated on every tick
	Date   time.Time // the calendar date this candle belongs to (midnight, local time)
	Ticks  int       // number of ticks received today (useful for debugging / thin-market detection)
}

// IntradayCache is a concurrency-safe, self-resetting intraday OHLC tracker.
// It maintains one IntradayCandle per instrument token and auto-resets it when
// a tick arrives on a new calendar day — no external reset job required.
//
// The cache is embedded inside MarketContext so strategies can access it via
// ctx.Intraday without any changes to the Strategy interface signature.
//
// Concurrency model:
//   - UpdateAndGet() is the single entry point used by the Processor before
//     calling strat.Evaluate(). It acquires an exclusive write lock to update
//     state, then returns a value-copy snapshot that strategies can read freely
//     without holding any lock.
//   - Get() is a read-only path for diagnostic or cross-strategy use.
type IntradayCache struct {
	mu   sync.RWMutex
	data map[uint32]*IntradayCandle
}

// NewIntradayCache constructs an empty IntradayCache ready for concurrent use.
func NewIntradayCache() *IntradayCache {
	return &IntradayCache{
		data: make(map[uint32]*IntradayCandle),
	}
}

// UpdateAndGet merges the tick price into the running intraday candle for the
// given token and returns a value-copy of the updated candle.
//
// Auto-reset: if the stored candle belongs to a previous calendar day, it is
// discarded and a fresh candle is started with the current tick price as Open.
//
// This is the primary call site — invoke it in Processor.processTick() before
// handing the tick to strategies.
func (ic *IntradayCache) UpdateAndGet(token uint32, price float64, tickTime time.Time) IntradayCandle {
	today := startOfDay(tickTime)

	ic.mu.Lock()
	defer ic.mu.Unlock()

	c, exists := ic.data[token]
	if !exists || c.Date.Before(today) {
		// First tick of the day (or ever) — open a fresh candle.
		ic.data[token] = &IntradayCandle{
			Open:  price,
			High:  price,
			Low:   price,
			Close: price,
			Date:  today,
			Ticks: 1,
		}
		return *ic.data[token]
	}

	// Update running OHLC.
	if price > c.High {
		c.High = price
	}
	if price < c.Low {
		c.Low = price
	}
	c.Close = price
	c.Ticks++

	return *c // return value copy — caller holds no reference into our map
}

// Get returns a value-copy of the current intraday candle for the given token.
// Returns (IntradayCandle{}, false) if no tick has been received today for this token.
func (ic *IntradayCache) Get(token uint32) (IntradayCandle, bool) {
	ic.mu.RLock()
	defer ic.mu.RUnlock()

	c, exists := ic.data[token]
	if !exists {
		return IntradayCandle{}, false
	}
	return *c, true
}

// Reset removes the intraday candle for a specific token.
// Useful for testing or manual eviction; normal day-boundary reset is automatic.
func (ic *IntradayCache) Reset(token uint32) {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	delete(ic.data, token)
}

// startOfDay returns midnight (00:00:00) of the given time in its local timezone.
func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}
