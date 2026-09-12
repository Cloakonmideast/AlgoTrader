package strategy

import (
	"testing"

	"github.com/zerodha/gokiteconnect/v4/models"

	"algotrader/internal/domain"
)

// countingStrategy records how often it was evaluated and emits a fixed signal,
// letting the tests assert both the filtering and the pass-through of every tick.
type countingStrategy struct {
	calls  int
	fire   bool
	signal string
}

func (c *countingStrategy) Name() string { return "Counter" }

func (c *countingStrategy) Evaluate(models.Tick, *domain.MarketContext) (bool, domain.OrderSignal) {
	c.calls++
	if !c.fire {
		return false, domain.OrderSignal{}
	}
	return true, domain.OrderSignal{StrategyName: c.signal}
}

func TestSignalFilterForwardsAllowedSignal(t *testing.T) {
	inner := &countingStrategy{fire: true, signal: "RSI (GFS)"}
	f := OnlySignals(inner, "RSI (GFS)", "RSI (Advanced GFS)")

	fired, sig := f.Evaluate(models.Tick{}, domain.NewMarketContext())
	if !fired {
		t.Fatal("an allowlisted signal must be forwarded")
	}
	if sig.StrategyName != "RSI (GFS)" {
		t.Errorf("signal name altered: got %q", sig.StrategyName)
	}
}

func TestSignalFilterBlocksOtherSignals(t *testing.T) {
	inner := &countingStrategy{fire: true, signal: "RSI (Support)"}
	f := OnlySignals(inner, "RSI (GFS)", "RSI (Advanced GFS)")

	if fired, _ := f.Evaluate(models.Tick{}, domain.NewMarketContext()); fired {
		t.Error("a signal outside the allowlist must not be forwarded")
	}
}

// The whole point of the wrapper is that it filters the OUTPUT, never the input:
// RSI's range-shift machines must observe every tick or they desync from price.
func TestSignalFilterAlwaysEvaluatesInner(t *testing.T) {
	inner := &countingStrategy{fire: true, signal: "RSI (Support)"} // always blocked
	f := OnlySignals(inner, "RSI (GFS)")

	ctx := domain.NewMarketContext()
	for i := 0; i < 5; i++ {
		f.Evaluate(models.Tick{}, ctx)
	}

	if inner.calls != 5 {
		t.Errorf("inner strategy must run on every tick even when filtered out: got %d of 5", inner.calls)
	}
}

func TestSignalFilterMatchingIsCaseInsensitive(t *testing.T) {
	inner := &countingStrategy{fire: true, signal: "RSI (Advanced GFS)"}
	f := OnlySignals(inner, "rsi (advanced gfs)")

	if fired, _ := f.Evaluate(models.Tick{}, domain.NewMarketContext()); !fired {
		t.Error("allowlist matching should ignore case")
	}
}

// An empty allowlist is an explicit "subscribe to nothing", not a firehose.
func TestSignalFilterEmptyAllowlistBlocksEverything(t *testing.T) {
	inner := &countingStrategy{fire: true, signal: "RSI (GFS)"}
	f := OnlySignals(inner)

	if fired, _ := f.Evaluate(models.Tick{}, domain.NewMarketContext()); fired {
		t.Error("an empty allowlist must forward nothing")
	}
	if inner.calls != 1 {
		t.Error("inner strategy must still be evaluated with an empty allowlist")
	}
}

// The wrapper must report the inner name so logs, the StateRegistry scope and
// Discord alerts all read as the real strategy.
func TestSignalFilterReportsInnerName(t *testing.T) {
	f := OnlySignals(&countingStrategy{}, "anything")
	if f.Name() != "Counter" {
		t.Errorf("Name() should delegate to the wrapped strategy, got %q", f.Name())
	}
}
