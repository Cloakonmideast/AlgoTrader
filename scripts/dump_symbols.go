//go:build ignore

// dump_symbols.go — one-shot script to print all distinct trading symbols
// that have candle data in the DB. Run with:
//
//	go run ./scripts/dump_symbols.go
package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	dbPath := os.Getenv("DATABASE_PATH")
	if dbPath == "" {
		dbPath = "market_data.db"
	}

	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?mode=ro&_journal_mode=WAL", dbPath))
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT DISTINCT trading_symbol FROM instruments
	                        WHERE exchange = 'NSE' AND instrument_type = 'EQ'
	                        ORDER BY trading_symbol`)
	if err != nil {
		// Try alternate schema
		rows, err = db.Query(`SELECT DISTINCT name FROM candles ORDER BY name`)
		if err != nil {
			log.Fatalf("query: %v", err)
		}
	}
	defer rows.Close()

	for rows.Next() {
		var sym string
		if err := rows.Scan(&sym); err != nil {
			continue
		}
		fmt.Println(sym)
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("rows: %v", err)
	}
}
