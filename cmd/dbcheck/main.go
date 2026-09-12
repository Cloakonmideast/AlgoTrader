package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	dbPath := "market_data.db"
	if len(os.Args) > 1 {
		dbPath = os.Args[1]
	}

	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?_journal_mode=WAL&mode=ro", dbPath))
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer db.Close()

	var latestDate, earliestDate string
	var totalRows, totalTokens int

	db.QueryRow(`SELECT MAX(date), MIN(date), COUNT(*), COUNT(DISTINCT token) FROM daily_candles`).
		Scan(&latestDate, &earliestDate, &totalRows, &totalTokens)

	fmt.Printf("Latest  date : %s\n", latestDate)
	fmt.Printf("Earliest date: %s\n", earliestDate)
	fmt.Printf("Total rows   : %d\n", totalRows)
	fmt.Printf("Total tokens : %d\n", totalTokens)

	// Show the 5 most recently updated tokens
	rows, err := db.Query(`
		SELECT token, MAX(date) as latest
		FROM daily_candles
		GROUP BY token
		ORDER BY latest DESC
		LIMIT 5`)
	if err != nil {
		log.Fatalf("query: %v", err)
	}
	defer rows.Close()

	fmt.Println("\nTop 5 tokens by latest candle date:")
	for rows.Next() {
		var token uint32
		var date string
		rows.Scan(&token, &date)
		fmt.Printf("  token=%-10d  date=%s\n", token, date)
	}
}
