package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"golang.org/x/time/rate"

	"algotrader/internal/domain"
)

// Client is the Discord webhook transport layer.
// It sends trade signal notifications and system alerts as richly formatted embed messages
// to a configured Discord channel via the Discord Incoming Webhook API.
//
// A token-bucket rate limiter enforces Discord's webhook quota of 5 requests per
// 2 seconds. All calls to SendSignal and SendAlert block until a slot is available,
// preventing HTTP 429 errors when many signals fire simultaneously.
type Client struct {
	webhookURL string
	httpClient *http.Client
	limiter    *rate.Limiter
}

// discordWebhookRPS is Discord's documented rate limit for webhook POSTs:
// 5 requests per 2 seconds = 2.5 req/s. We use 2 req/s to stay comfortably
// under the limit and avoid 429s from accumulated bursts.
const discordWebhookRPS = 2

// NewClient constructs a Discord Client with the given webhook URL.
// The internal HTTP client is pre-configured with a 10-second timeout to prevent
// indefinite blocking in the dispatch goroutine during network degradation.
func NewClient(webhookURL string) *Client {
	return &Client{
		webhookURL: webhookURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		// Burst of 5 allows a small initial burst; steady-state is 2 req/s.
		limiter: rate.NewLimiter(rate.Limit(discordWebhookRPS), 5),
	}
}

// webhookPayload is the top-level Discord webhook request body.
type webhookPayload struct {
	Username string  `json:"username"`
	Embeds   []embed `json:"embeds"`
}

// embed represents a single Discord message embed with color-coded styling.
type embed struct {
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Color       int     `json:"color"`
	Fields      []field `json:"fields"`
	Footer      footer  `json:"footer"`
	Timestamp   string  `json:"timestamp"`
}

// field is a name-value pair within a Discord embed.
type field struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

// footer appears at the bottom of a Discord embed.
type footer struct {
	Text string `json:"text"`
}

const (
	// colorGreen is the Discord embed color for LONG signals (hex #00C853 → decimal).
	colorGreen = 0x00C853
	// colorRed is the Discord embed color for SHORT signals (hex #D50000 → decimal).
	colorRed = 0xD50000
	// colorBlue is the Discord embed color for system/informational alerts.
	colorBlue = 0x2979FF
	// colorOrange is used for warning alerts.
	colorOrange = 0xFF6D00
)

func (c *Client) SendSignal(signal domain.OrderSignal) error {
	payload := webhookPayload{
		Username: "AlgoTrader 🤖",
		Embeds: []embed{
			{
				Title:       fmt.Sprintf("📊 %s", signal.TradingSymbol),
				Description: fmt.Sprintf("**%s** triggered the **%s** strategy.", signal.TradingSymbol, signal.StrategyName),
				Color:       colorGreen,
				Footer:      footer{Text: "AlgoTrader Engine"},
				Timestamp:   signal.SignalTime.Format(time.RFC3339),
			},
		},
	}
	return c.post(payload)
}

// SendAlert posts a plain informational or warning message to Discord.
// Used by the scheduler for lifecycle event notifications (boot start, EoD rollup complete, etc.).
func (c *Client) SendAlert(title, message string, isWarning bool) error {
	color := colorBlue
	icon := "ℹ️"
	if isWarning {
		color = colorOrange
		icon = "⚠️"
	}

	payload := webhookPayload{
		Username: "AlgoTrader 🤖",
		Embeds: []embed{
			{
				Title:       fmt.Sprintf("%s %s", icon, title),
				Description: message,
				Color:       color,
				Footer:      footer{Text: "AlgoTrader Engine"},
				Timestamp:   time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	return c.post(payload)
}

// post marshals the payload to JSON and sends an HTTP POST request to the Discord webhook URL.
// It waits for a rate-limit slot before each request, then on HTTP 429 reads Discord's
// retry_after field from the JSON body and sleeps for exactly that duration before retrying.
func (c *Client) post(payload webhookPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("discord: failed to marshal webhook payload: %w", err)
	}

	// Block until the rate limiter grants a slot.
	if err := c.limiter.Wait(context.Background()); err != nil {
		return fmt.Errorf("discord: rate limiter error: %w", err)
	}

	const maxRetries = 5
	for attempt := 1; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequest(http.MethodPost, c.webhookURL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("discord: failed to construct HTTP request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "AlgoTrader/1.0")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("discord: HTTP request failed: %w", err)
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		// Discord returns 204 No Content on success for webhook posts.
		if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
			log.Printf("[discord] Webhook delivered successfully (status: %d)", resp.StatusCode)
			return nil
		}

		// On 429 Too Many Requests, read Discord's retry_after and sleep exactly that long.
		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries {
			// Discord returns: {"retry_after": 1.234, "global": false, ...}
			var rl struct {
				RetryAfter float64 `json:"retry_after"`
			}
			backoff := time.Duration(attempt) * time.Second // safe fallback
			if jsonErr := json.Unmarshal(respBody, &rl); jsonErr == nil && rl.RetryAfter > 0 {
				// Add 50ms headroom on top of Discord's requested delay.
				backoff = time.Duration(rl.RetryAfter*1000)*time.Millisecond + 50*time.Millisecond
			}
			log.Printf("[discord] Rate limited (429), sleeping %s before retry %d/%d", backoff, attempt, maxRetries)
			time.Sleep(backoff)
			continue
		}

		return fmt.Errorf("discord: webhook returned unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	return fmt.Errorf("discord: webhook failed after %d retries", maxRetries)
}

// SignalSender is the minimal interface required to deliver an OrderSignal
// notification. Implemented by *Client (one-way webhook) and by *BotClient
// (interactive buttons + modal). This keeps the Processor decoupled from
// the concrete Discord transport being used.
type SignalSender interface {
	SendSignal(signal domain.OrderSignal) error
}
