//go:build ignore

package main

import (
	"database/sql"
	"fmt"
	"log"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	db, err := sql.Open("sqlite3", "file:market_data.db?_journal_mode=WAL&_synchronous=NORMAL")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	var maxDate string
	db.QueryRow("SELECT MAX(date) FROM daily_candles").Scan(&maxDate)
	fmt.Println("Latest date in DB:", maxDate)

	var count int
	db.QueryRow("SELECT COUNT(DISTINCT date) FROM daily_candles").Scan(&count)
	fmt.Println("Total trading days:", count)

	var tokens int
	db.QueryRow("SELECT COUNT(DISTINCT token) FROM daily_candles").Scan(&tokens)
	fmt.Println("Total tokens:", tokens)
}
