//go:build ignore

// gen_nifty500_embed.go — one-shot codegen: fetches the Zerodha instrument master,
// cross-references it with the 500 tokens stored in market_data.db, and writes
// internal/service/nifty500_symbols_embed.go with a hardcoded symbol set.
//
// Run once (needs valid KITE_API_KEY + KITE_ACCESS_TOKEN in env):
//
//	go run ./scripts/gen_nifty500_embed.go
package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	_ "github.com/mattn/go-sqlite3"
	kiteconnect "github.com/zerodha/gokiteconnect/v4"
)

func main() {
	apiKey := mustEnv("KITE_API_KEY")
	accessToken := mustEnv("KITE_ACCESS_TOKEN")
	dbPath := getenvOr("DATABASE_PATH", "market_data.db")

	// ── 1. Load the 500 tokens from the DB ────────────────────────────────────
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?mode=ro&_journal_mode=WAL", dbPath))
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT DISTINCT token FROM daily_candles`)
	if err != nil {
		log.Fatalf("query tokens: %v", err)
	}
	dbTokens := make(map[uint32]bool, 500)
	for rows.Next() {
		var tok uint32
		if err := rows.Scan(&tok); err != nil {
			continue
		}
		dbTokens[tok] = true
	}
	rows.Close()
	log.Printf("Loaded %d tokens from DB", len(dbTokens))

	// ── 2. Fetch Zerodha instrument master ────────────────────────────────────
	kc := kiteconnect.New(apiKey)
	kc.SetAccessToken(accessToken)

	instruments, err := kc.GetInstrumentsByExchange("NSE")
	if err != nil {
		log.Fatalf("GetInstruments(NSE): %v", err)
	}
	log.Printf("Fetched %d NSE instruments from Zerodha", len(instruments))

	// ── 3. Cross-reference ────────────────────────────────────────────────────
	symbols := make([]string, 0, 500)
	for _, inst := range instruments {
		if inst.InstrumentType != "EQ" || inst.Segment != "NSE" {
			continue
		}
		if dbTokens[uint32(inst.InstrumentToken)] {
			symbols = append(symbols, inst.Tradingsymbol)
		}
	}
	sort.Strings(symbols)
	log.Printf("Matched %d symbols", len(symbols))

	// ── 4. Write the embed file ───────────────────────────────────────────────
	var b strings.Builder
	b.WriteString("package service\n\n")
	b.WriteString("// nifty500EmbeddedSymbols is a static snapshot of the Nifty 500 constituent\n")
	b.WriteString("// trading symbols, used as a fallback when the live NSE CSV download fails.\n")
	b.WriteString("// Regenerate with: go run ./scripts/gen_nifty500_embed.go\n")
	b.WriteString("var nifty500EmbeddedSymbols = []string{\n")
	for _, sym := range symbols {
		fmt.Fprintf(&b, "\t%q,\n", sym)
	}
	b.WriteString("}\n")

	out := "internal/service/nifty500_symbols_embed.go"
	if err := os.WriteFile(out, []byte(b.String()), 0644); err != nil {
		log.Fatalf("write %s: %v", out, err)
	}
	log.Printf("Written %s with %d symbols", out, len(symbols))
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if strings.TrimSpace(v) == "" {
		log.Fatalf("required env var %q not set", key)
	}
	return v
}

func getenvOr(key, fallback string) string {
	if v := os.Getenv(key); strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}
