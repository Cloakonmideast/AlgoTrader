package strategy

import (
	"github.com/zerodha/gokiteconnect/v4/models"

	"algotrader/internal/domain"
)

// Strategy is the unified interface that all trading strategy implementations must satisfy.
// The engine evaluates each registered Strategy on every incoming market tick.
//
// Contract:
//   - Name() must return a stable, human-readable identifier (used in logs and Discord alerts).
//   - Evaluate() must be stateless with respect to shared mutable state; it receives a deep copy
//     of historical candles via MarketContext.Get() and must not retain references across calls.
//   - Evaluate() must be safe for concurrent invocation from multiple worker goroutines.
//   - Evaluate() returns (false, zero-value) when no entry signal is detected.
//   - Evaluate() returns (true, populated OrderSignal) when an entry condition is met.
type Strategy interface {
	// Name returns the human-readable strategy identifier used in logs and notifications.
	Name() string

	// Evaluate analyses the incoming tick against the strategy's decision rules using the
	// historical market data available in the MarketContext RAM cache.
	// Returns (true, signal) if an entry condition is triggered, (false, zero) otherwise.
	Evaluate(tick models.Tick, context *domain.MarketContext) (bool, domain.OrderSignal)
}
