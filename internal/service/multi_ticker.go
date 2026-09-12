package service

import (
	"fmt"
	"log"
	"time"

	"github.com/zerodha/gokiteconnect/v4/models"
	kiteticker "github.com/zerodha/gokiteconnect/v4/ticker"
)

const (
	// numWebSockets is the number of parallel Zerodha WebSocket connections to maintain.
	// Zerodha allows up to 3 WebSocket connections per API key, each supporting up to
	// 3,000 instrument subscriptions — giving a theoretical ceiling of 9,000 instruments.
	numWebSockets = 3
)

// MultiTicker manages numWebSockets parallel Zerodha WebSocket connections, distributing
// instrument tokens evenly across them. This provides:
//   - Higher aggregate tick throughput (3× parallel streams)
//   - Fault isolation: a single WS drop only affects ~1/3 of instruments during reconnect
//   - Full utilisation of Zerodha's per-API-key WebSocket quota
//
// All ticks from all connections are delivered to the single onTick callback, making
// the MultiTicker a transparent drop-in for a single kiteticker.Ticker from the
// perspective of downstream consumers (e.g. the tick processor).
type MultiTicker struct {
	tickers     [numWebSockets]*kiteticker.Ticker
	partitions  [numWebSockets][]uint32
	onTick      func(models.Tick)
	apiKey      string
	accessToken string
	discord     discordAlerter
}

// discordAlerter is a minimal interface satisfied by the Discord client, allowing
// MultiTicker to send alerts without importing the full transport package.
type discordAlerter interface {
	SendAlert(title, body string, isError bool) error
}

// NewMultiTicker constructs a MultiTicker that distributes tokens across numWebSockets
// connections. Tokens are partitioned by index modulo (token[i] → WS[i%3]), which
// spreads alphabetically diverse stocks across all connections.
func NewMultiTicker(
	apiKey, accessToken string,
	tokens []uint32,
	onTick func(models.Tick),
	discord discordAlerter,
) *MultiTicker {
	mt := &MultiTicker{
		onTick:      onTick,
		apiKey:      apiKey,
		accessToken: accessToken,
		discord:     discord,
	}

	// Partition tokens round-robin across the numWebSockets slots.
	for i, tok := range tokens {
		slot := i % numWebSockets
		mt.partitions[slot] = append(mt.partitions[slot], tok)
	}

	for i := 0; i < numWebSockets; i++ {
		log.Printf("[multi_ticker] WS[%d] assigned %d tokens", i, len(mt.partitions[i]))
	}

	return mt
}

// Start initialises and connects all numWebSockets WebSocket tickers concurrently.
// Each ticker is fully independent — reconnection, subscription, and error handling
// are all managed per-connection without affecting siblings.
func (mt *MultiTicker) Start() {
	for i := 0; i < numWebSockets; i++ {
		mt.tickers[i] = mt.buildTicker(i)
	}

	for i := 0; i < numWebSockets; i++ {
		idx := i // capture for goroutine closure
		go func() {
			log.Printf("[multi_ticker] WS[%d] connecting...", idx)
			mt.tickers[idx].Serve()
		}()
	}
}

// Stop gracefully closes all WebSocket connections.
func (mt *MultiTicker) Stop() {
	for i := 0; i < numWebSockets; i++ {
		if mt.tickers[i] != nil {
			if err := mt.tickers[i].Close(); err != nil {
				log.Printf("[multi_ticker] WS[%d] close error: %v", i, err)
			} else {
				log.Printf("[multi_ticker] WS[%d] closed", i)
			}
		}
	}
}

// buildTicker creates and wires a single kiteticker.Ticker for the given partition index.
func (mt *MultiTicker) buildTicker(idx int) *kiteticker.Ticker {
	partition := mt.partitions[idx]
	ticker := kiteticker.New(mt.apiKey, mt.accessToken)

	ticker.OnConnect(func() {
		log.Printf("[multi_ticker] WS[%d] connected — subscribing to %d tokens", idx, len(partition))

		tokens := make([]uint32, len(partition))
		copy(tokens, partition)

		if err := ticker.Subscribe(tokens); err != nil {
			log.Printf("[multi_ticker] WS[%d] ERROR subscribing: %v", idx, err)
			return
		}
		if err := ticker.SetMode(kiteticker.ModeFull, tokens); err != nil {
			log.Printf("[multi_ticker] WS[%d] ERROR setting FULL mode: %v", idx, err)
			return
		}
		log.Printf("[multi_ticker] WS[%d] subscribed to %d tokens in FULL mode", idx, len(tokens))

		_ = mt.discord.SendAlert(
			fmt.Sprintf("AlgoTrader WS[%d] Online", idx),
			fmt.Sprintf("WebSocket %d connected. Monitoring %d instruments.", idx, len(tokens)),
			false,
		)
	})

	ticker.OnTick(func(tick models.Tick) {
		mt.onTick(tick)
	})

	ticker.OnError(func(err error) {
		log.Printf("[multi_ticker] WS[%d] error: %v", idx, err)
		_ = mt.discord.SendAlert(
			fmt.Sprintf("WS[%d] Error", idx),
			err.Error(),
			true,
		)
	})

	ticker.OnClose(func(code int, reason string) {
		log.Printf("[multi_ticker] WS[%d] closed: code=%d reason=%s", idx, code, reason)
	})

	ticker.OnReconnect(func(attempt int, delay time.Duration) {
		log.Printf("[multi_ticker] WS[%d] reconnecting: attempt=%d delay=%s", idx, attempt, delay)
	})

	ticker.OnNoReconnect(func(attempt int) {
		log.Printf("[multi_ticker] WS[%d] giving up after %d reconnect attempts", idx, attempt)
		_ = mt.discord.SendAlert(
			fmt.Sprintf("WS[%d] Disconnected", idx),
			fmt.Sprintf("WebSocket %d lost connection after %d attempts.", idx, attempt),
			true,
		)
	})

	return ticker
}
