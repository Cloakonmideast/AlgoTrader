package domain

import (
	"fmt"
	"sync"
	"time"
)

// OrderSignal is the output produced by a strategy when it detects a valid entry condition.
// It carries all information needed to construct a notification.
type OrderSignal struct {
	// InstrumentToken is the Zerodha numeric identifier for the tradeable instrument.
	InstrumentToken uint32

	// TradingSymbol is the human-readable exchange symbol (e.g., "RELIANCE", "NIFTY23JUNFUT").
	TradingSymbol string

	// StrategyName is the display name of the strategy that generated this signal.
	StrategyName string

	// SignalTime is the UTC timestamp when this signal was generated.
	SignalTime time.Time
}

// String returns a human-readable representation of the OrderSignal for logging and notifications.
func (o OrderSignal) String() string {
	return fmt.Sprintf(
		"[%s] %s | Time: %s",
		o.StrategyName,
		o.TradingSymbol,
		o.SignalTime.Format(time.RFC3339),
	)
}

// StateRegistry provides a thread-safe mechanism to track which instrument tokens
// have already fired a signal today. This prevents duplicate alerts and enforces a
// one-alert-per-stock-per-day rule.
//
// Locks are SCOPED: the key is (scope, token), where scope is normally the strategy
// name. Without that scoping a chatty strategy starves a selective one — an Ichimoku
// alert at 09:30 would silently suppress a GFS alert on the same stock at 14:00, and
// that GFS signal would never be delivered. Each scope now gets its own daily slot
// per token.
//
// The lock expires automatically at the start of a new calendar day in the local
// timezone (Asia/Kolkata).
type StateRegistry struct {
	mu     sync.RWMutex
	active map[lockKey]time.Time
}

// lockKey identifies one daily alert slot: a token within a single scope.
type lockKey struct {
	scope string
	token uint32
}

// NewStateRegistry constructs an empty StateRegistry ready for concurrent use.
func NewStateRegistry() *StateRegistry {
	return &StateRegistry{
		active: make(map[lockKey]time.Time),
	}
}

// istNow returns the current time in Asia/Kolkata, falling back to a fixed +05:30
// zone when the tzdata lookup fails.
func istNow() time.Time {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		loc = time.FixedZone("IST", 5*3600+30*60)
	}
	return time.Now().In(loc)
}

// sameDay reports whether two instants fall on the same calendar day in loc.
func sameDay(a, b time.Time, loc *time.Location) bool {
	x, y := a.In(loc), b.In(loc)
	return x.Year() == y.Year() && x.Month() == y.Month() && x.Day() == y.Day()
}

// TryLock attempts to acquire today's alert slot for the given scope and token.
// Returns true if this scope has NOT yet alerted on the token today (and takes the
// slot). Returns false if it already has.
func (s *StateRegistry) TryLock(scope string, token uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := istNow()
	key := lockKey{scope: scope, token: token}

	if lockTime, exists := s.active[key]; exists && sameDay(lockTime, now, now.Location()) {
		return false // this scope already alerted on this token today
	}

	s.active[key] = now
	return true
}

// IsLocked performs a read-only check to determine whether a scope has already
// alerted on a token today. Useful for diagnostics without taking a write lock.
func (s *StateRegistry) IsLocked(scope string, token uint32) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := istNow()
	lockTime, exists := s.active[lockKey{scope: scope, token: token}]
	return exists && sameDay(lockTime, now, now.Location())
}

// ActiveCount returns the total number of tokens that have fired an alert.
// Note: this returns the total historically tracked locks, not just today's.
func (s *StateRegistry) ActiveCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.active)
}
