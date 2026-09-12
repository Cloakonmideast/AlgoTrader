package domain

import (
	"sync"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
)

// MarketContext is the in-process RAM cache for all market data available to strategies.
// It combines two distinct data sources:
//
//  1. Historical daily candles (data): a 3-year rolling window loaded from SQLite at boot.
//     Keyed by instrument token; slices are sorted chronologically ascending.
//
//  2. Intraday OHLC (Intraday): a live, tick-by-tick candle built for the current trading
//     day. Auto-resets at day boundaries — no external reset job required.
//
// Both fields are individually concurrency-safe. Strategies access historical data via
// Get() and live intraday data via Intraday.Get() or ctx.Intraday directly.
type MarketContext struct {
	mu       sync.RWMutex
	data     map[uint32][]kiteconnect.HistoricalData

	// Intraday is the live tick-by-tick OHLC tracker for the current trading day.
	// It is safe for concurrent use and is updated by the Processor on every tick
	// before strategy evaluation. Strategies should call Intraday.Get(token) to
	// read today's running Open, High, Low, Close, and tick count.
	Intraday *IntradayCache
}

// NewMarketContext constructs an empty, concurrency-safe MarketContext with both
// a historical candle cache and a live intraday OHLC tracker ready for use.
func NewMarketContext() *MarketContext {
	return &MarketContext{
		data:     make(map[uint32][]kiteconnect.HistoricalData),
		Intraday: NewIntradayCache(),
	}
}

// Set replaces the full historical candle slice for a given instrument token.
// This method acquires an exclusive write lock, making it safe to call concurrently
// from the bootloader goroutine while strategy workers are actively reading.
func (mc *MarketContext) Set(token uint32, candles []kiteconnect.HistoricalData) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	// Store a deep copy so the caller's slice cannot affect our master dataset.
	copied := make([]kiteconnect.HistoricalData, len(candles))
	copy(copied, candles)
	mc.data[token] = copied
}

// Append adds a single new candle to the end of an instrument's historical slice.
// It is used by the end-of-day rollup routine to extend the cache with today's
// freshly closed candle without requiring a full cache reload.
func (mc *MarketContext) Append(token uint32, candle kiteconnect.HistoricalData) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.data[token] = append(mc.data[token], candle)
}

// Get returns a deep copy of the historical candle slice for a given instrument token.
// Returns nil and false if the token has no data loaded in cache.
//
// Deep copying is essential: strategy evaluation routines may perform slice operations
// (sorting, filtering, appending) on the returned data; without a copy they would race
// against the bootloader's write path or corrupt the master dataset for other goroutines.
func (mc *MarketContext) Get(token uint32) ([]kiteconnect.HistoricalData, bool) {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	src, exists := mc.data[token]
	if !exists {
		return nil, false
	}

	// Return a deep copy to protect the master memory space.
	dst := make([]kiteconnect.HistoricalData, len(src))
	copy(dst, src)
	return dst, true
}

// Delete removes all cached historical data for a given instrument token.
// Useful for cleanly evicting a token that is no longer being tracked.
func (mc *MarketContext) Delete(token uint32) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	delete(mc.data, token)
}

// TokenCount returns the number of instrument tokens currently loaded in the cache.
func (mc *MarketContext) TokenCount() int {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	return len(mc.data)
}

// CandleCount returns the number of historical candles loaded for a specific token.
// Returns 0 if the token is not present in the cache.
func (mc *MarketContext) CandleCount(token uint32) int {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	return len(mc.data[token])
}

// Tokens returns a snapshot of all instrument tokens currently present in the cache.
func (mc *MarketContext) Tokens() []uint32 {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	tokens := make([]uint32, 0, len(mc.data))
	for t := range mc.data {
		tokens = append(tokens, t)
	}
	return tokens
}
