package strategy

import (
	"strings"

	"github.com/zerodha/gokiteconnect/v4/models"

	"algotrader/internal/domain"
)

// SignalFilter wraps a Strategy and forwards only the signals whose names appear
// in an allowlist. Everything else is evaluated and then discarded.
//
// The point is alert volume. A strategy like RSI emits a dozen signal variants and
// fires tens of thousands of times a year across the universe; registering it
// directly would bury the handful of setups actually worth acting on. Wrapping it
// lets the engine subscribe to just those.
//
// The inner strategy is ALWAYS evaluated, never short-circuited. RSI's range-shift
// state machines have to observe every tick to stay in sync with price — filtering
// before evaluation would desync them and corrupt the very signals being kept.
type SignalFilter struct {
	inner Strategy
	allow map[string]struct{}
}

// OnlySignals wraps inner so that only signals whose StrategyName exactly matches
// one of names is forwarded. Matching is case-insensitive.
//
// With no names given, nothing is forwarded — an explicit "subscribe to nothing"
// rather than an accidental firehose.
func OnlySignals(inner Strategy, names ...string) *SignalFilter {
	allow := make(map[string]struct{}, len(names))
	for _, n := range names {
		allow[strings.ToLower(strings.TrimSpace(n))] = struct{}{}
	}
	return &SignalFilter{inner: inner, allow: allow}
}

// Name returns the wrapped strategy's name, so logs, the StateRegistry scope, and
// Discord alerts all read as the underlying strategy rather than the wrapper.
func (f *SignalFilter) Name() string { return f.inner.Name() }

// Allows reports whether a signal name would be forwarded. Exposed for startup
// logging so a typo in the allowlist is visible at boot rather than as silence.
func (f *SignalFilter) Allows(signalName string) bool {
	_, ok := f.allow[strings.ToLower(strings.TrimSpace(signalName))]
	return ok
}

// Evaluate runs the wrapped strategy and passes the signal through only when its
// name is allowed.
func (f *SignalFilter) Evaluate(tick models.Tick, ctx *domain.MarketContext) (bool, domain.OrderSignal) {
	fired, signal := f.inner.Evaluate(tick, ctx)
	if !fired || !f.Allows(signal.StrategyName) {
		return false, domain.OrderSignal{}
	}
	return true, signal
}
