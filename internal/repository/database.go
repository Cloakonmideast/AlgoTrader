package repository

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3" // SQLite driver registration via side-effect import
	kiteconnect "github.com/zerodha/gokiteconnect/v4"
	kitemodels "github.com/zerodha/gokiteconnect/v4/models"
)

const (
	// createTableSQL defines the schema for the daily candles table.
	// The composite primary key (token, date) enforces uniqueness and enables fast range scans.
	createTableSQL = `
	CREATE TABLE IF NOT EXISTS daily_candles (
		token   INTEGER NOT NULL,
		date    DATETIME NOT NULL,
		open    REAL NOT NULL,
		high    REAL NOT NULL,
		low     REAL NOT NULL,
		close   REAL NOT NULL,
		volume  INTEGER NOT NULL,
		PRIMARY KEY (token, date)
	);`

	// createIndexSQL creates a covering index to accelerate token + date range queries.
	createIndexSQL = `
	CREATE INDEX IF NOT EXISTS idx_daily_candles_token_date ON daily_candles (token, date);`

	// createNoDataTableSQL tracks instrument tokens that consistently return zero candles
	// from the Zerodha API (delisted, suspended, or SME-only instruments).
	// Tokens in this table are permanently skipped during boot to avoid redundant API calls,
	// reducing second-boot time from ~25 minutes to near-instant.
	createNoDataTableSQL = `
	CREATE TABLE IF NOT EXISTS no_data_tokens (
		token       INTEGER PRIMARY KEY,
		recorded_at DATETIME NOT NULL
	);`
)

// retentionYears bounds how much history the engine loads into RAM and keeps on
// disk. It is read once from RETENTION_YEARS, defaulting to 3.
//
// Deepening it is a two-part change: raise this value AND backfill the extra
// years (`go run ./cmd/backfill -years N`). Raising it alone loads nothing new;
// backfilling alone leaves the data to be purged by the next EoD rollup.
var retentionYears = envIntOr("RETENTION_YEARS", 3)

// retentionModifier renders the window as an SQLite date() modifier, e.g. "-3 years".
func retentionModifier() string {
	return fmt.Sprintf("-%d years", retentionYears)
}

// RetentionYears reports the configured history window, for startup logging.
func RetentionYears() int { return retentionYears }

// envIntOr reads a positive integer from the environment, falling back on any
// missing, malformed, or non-positive value.
func envIntOr(key string, fallback int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

// DB wraps the underlying *sql.DB handle and exposes domain-specific data access methods.
// All public methods are safe for concurrent use as the underlying sql.DB is a connection pool.
type DB struct {
	conn *sql.DB
}

// NewDB opens (or creates) the SQLite database at the given file path, applies the schema,
// and configures connection pool settings appropriate for a single-file SQLite workload.
// It returns an error if the file cannot be opened or the schema cannot be applied.
func NewDB(path string) (*DB, error) {
	conn, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?_journal_mode=WAL&_synchronous=NORMAL&_cache_size=10000&_foreign_keys=on", path))
	if err != nil {
		return nil, fmt.Errorf("repository: failed to open sqlite database at %q: %w", path, err)
	}

	// SQLite in WAL mode supports concurrent readers; allow up to 4 open connections
	// so the 3-worker bootloader can read in parallel. Writes still serialize via WAL.
	conn.SetMaxOpenConns(4)
	conn.SetMaxIdleConns(4)
	conn.SetConnMaxLifetime(0) // Connections live indefinitely; SQLite has no network timeout.

	if err := conn.Ping(); err != nil {
		return nil, fmt.Errorf("repository: database ping failed: %w", err)
	}

	db := &DB{conn: conn}
	if err := db.migrate(); err != nil {
		return nil, fmt.Errorf("repository: schema migration failed: %w", err)
	}

	log.Printf("[repository] SQLite database opened and schema applied: %s", path)
	// Surfaced at boot because the EoD rollup DELETES everything outside this
	// window — a RETENTION_YEARS that failed to reach the process would silently
	// purge years of backfilled history on the first rollup.
	log.Printf("[repository] History retention window: %d years (RETENTION_YEARS)", retentionYears)
	return db, nil
}

// migrate applies the initial schema and indexes using a single transaction.
func (db *DB) migrate() error {
	tx, err := db.conn.Begin()
	if err != nil {
		return fmt.Errorf("migrate: begin transaction: %w", err)
	}
	// Idiomatic safety net: fires on any return path; no-op after Commit() (returns sql.ErrTxDone).
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(createTableSQL); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("migrate: create daily_candles table: %w", err)
	}
	if _, err := tx.Exec(createIndexSQL); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("migrate: create index: %w", err)
	}
	if _, err := tx.Exec(createNoDataTableSQL); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("migrate: create no_data_tokens table: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate: commit: %w", err)
	}
	return nil
}

// LoadHistory fetches all daily candle rows for the given instrument token that fall
// within the rolling retention window (RETENTION_YEARS, default 3), ordered
// chronologically ascending.
// It returns the rows as a slice of kiteconnect.HistoricalData for direct cache loading.
func (db *DB) LoadHistory(token uint32) ([]kiteconnect.HistoricalData, error) {
	const query = `
		SELECT date, open, high, low, close, volume
		FROM daily_candles
		WHERE token = ?
		  AND date >= date('now', ?)
		ORDER BY date ASC`

	rows, err := db.conn.Query(query, token, retentionModifier())
	if err != nil {
		return nil, fmt.Errorf("repository: LoadHistory query for token %d: %w", token, err)
	}
	defer rows.Close()

	var candles []kiteconnect.HistoricalData
	for rows.Next() {
		var (
			dateStr string
			open    float64
			high    float64
			low     float64
			close   float64
			volume  int64
		)
		if err := rows.Scan(&dateStr, &open, &high, &low, &close, &volume); err != nil {
			return nil, fmt.Errorf("repository: LoadHistory scan for token %d: %w", token, err)
		}
		t, err := time.Parse(time.RFC3339, dateStr)
		if err != nil {
			// Fallback: try parsing as a bare date (SQLite date() returns YYYY-MM-DD).
			t, err = time.Parse("2006-01-02", dateStr)
			if err != nil {
				return nil, fmt.Errorf("repository: LoadHistory parse date %q for token %d: %w", dateStr, token, err)
			}
		}
		candles = append(candles, kiteconnect.HistoricalData{
			Date:   kitemodels.Time{Time: t},
			Open:   open,
			High:   high,
			Low:    low,
			Close:  close,
			Volume: int(volume),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repository: LoadHistory rows iteration for token %d: %w", token, err)
	}
	return candles, nil
}

// CountHistory returns the number of daily candle rows stored for the given token
// within the retention window. Used by the bootloader to detect whether a full API
// fetch is required on first boot.
func (db *DB) CountHistory(token uint32) (int, error) {
	const query = `
		SELECT COUNT(*)
		FROM daily_candles
		WHERE token = ?
		  AND date >= date('now', ?)`

	var count int
	err := db.conn.QueryRow(query, token, retentionModifier()).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("repository: CountHistory for token %d: %w", token, err)
	}
	return count, nil
}

// SaveCandles persists a batch of historical candles for the given token inside a single
// transaction. It uses INSERT OR REPLACE to provide idempotent upsert semantics,
// making it safe to call on data that may already partially exist in the database.
func (db *DB) SaveCandles(token uint32, candles []kiteconnect.HistoricalData) error {
	if len(candles) == 0 {
		return nil
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return fmt.Errorf("repository: SaveCandles begin transaction for token %d: %w", token, err)
	}
	// Idiomatic safety net: fires on any return path; no-op after Commit() (returns sql.ErrTxDone).
	defer func() { _ = tx.Rollback() }()

	const insertSQL = `
		INSERT OR REPLACE INTO daily_candles (token, date, open, high, low, close, volume)
		VALUES (?, ?, ?, ?, ?, ?, ?)`

	stmt, err := tx.Prepare(insertSQL)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("repository: SaveCandles prepare statement for token %d: %w", token, err)
	}
	defer stmt.Close()

	for _, c := range candles {
		dateStr := c.Date.Time.UTC().Format(time.RFC3339)
		if _, err := stmt.Exec(token, dateStr, c.Open, c.High, c.Low, c.Close, c.Volume); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("repository: SaveCandles insert for token %d at date %s: %w", token, dateStr, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("repository: SaveCandles commit for token %d: %w", token, err)
	}
	log.Printf("[repository] Saved %d candles for token %d", len(candles), token)
	return nil
}

// DropOldData removes all daily candle rows older than the retention window
// (RETENTION_YEARS, default 3). This is called by the end-of-day rollup scheduler
// to keep the database bounded and prevent unbounded growth of the SQLite file.
//
// NOTE: this is destructive and runs daily. Backfilling deeper history without
// also raising RETENTION_YEARS will have that history deleted the same evening.
func (db *DB) DropOldData() error {
	const deleteSQL = `DELETE FROM daily_candles WHERE date < date('now', ?)`

	result, err := db.conn.Exec(deleteSQL, retentionModifier())
	if err != nil {
		return fmt.Errorf("repository: DropOldData exec: %w", err)
	}
	affected, _ := result.RowsAffected()
	log.Printf("[repository] DropOldData purged %d rows older than %d years", affected, retentionYears)
	return nil
}

// CleanupDelisted removes historical candle data and skip-list entries for any instrument
// token that is no longer present in the provided activeTokens slice (i.e., tokens that
// have been delisted, suspended, or removed from the NSE EQ segment).
//
// It uses a temporary table populated in a single transaction to perform an efficient
// bulk DELETE … WHERE token NOT IN (SELECT token FROM active) without generating a
// huge SQL IN-clause literal.
//
// Returns the number of candle rows deleted.
func (db *DB) CleanupDelisted(activeTokens []uint32) (int64, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return 0, fmt.Errorf("repository: CleanupDelisted begin: %w", err)
	}
	// Idiomatic safety net: fires on any return path; no-op after Commit() (returns sql.ErrTxDone).
	defer func() { _ = tx.Rollback() }()

	// Create a temporary table that lives only for this transaction.
	if _, err := tx.Exec(`CREATE TEMPORARY TABLE IF NOT EXISTS _active_tokens (token INTEGER PRIMARY KEY)`); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("repository: CleanupDelisted create temp table: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM _active_tokens`); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("repository: CleanupDelisted clear temp table: %w", err)
	}

	// Bulk-insert all active tokens into the temp table.
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO _active_tokens (token) VALUES (?)`)
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("repository: CleanupDelisted prepare insert: %w", err)
	}
	defer stmt.Close()
	for _, tok := range activeTokens {
		if _, err := stmt.Exec(tok); err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("repository: CleanupDelisted insert token %d: %w", tok, err)
		}
	}

	// Delete candle rows for tokens not in the active set.
	result, err := tx.Exec(`DELETE FROM daily_candles WHERE token NOT IN (SELECT token FROM _active_tokens)`)
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("repository: CleanupDelisted delete candles: %w", err)
	}
	deleted, _ := result.RowsAffected()

	// Also purge the no_data skip list for removed tokens — keeps both tables consistent.
	if _, err := tx.Exec(`DELETE FROM no_data_tokens WHERE token NOT IN (SELECT token FROM _active_tokens)`); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("repository: CleanupDelisted delete no_data: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("repository: CleanupDelisted commit: %w", err)
	}
	return deleted, nil
}

// MarkNoData records a token in the no_data_tokens table, permanently flagging it
// as a zero-candle instrument (delisted, suspended, or no historical data available).
// Subsequent boots will skip the API call for this token entirely.
func (db *DB) MarkNoData(token uint32) error {
	const query = `
		INSERT OR IGNORE INTO no_data_tokens (token, recorded_at)
		VALUES (?, datetime('now'))`
	_, err := db.conn.Exec(query, token)
	if err != nil {
		return fmt.Errorf("repository: MarkNoData for token %d: %w", token, err)
	}
	return nil
}

// IsNoDataToken returns true if the token is recorded in the no_data_tokens skip list.
// Used by the bootloader to bypass the API call for known zero-candle instruments.
func (db *DB) IsNoDataToken(token uint32) (bool, error) {
	const query = `SELECT COUNT(1) FROM no_data_tokens WHERE token = ?`
	var count int
	err := db.conn.QueryRow(query, token).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("repository: IsNoDataToken for token %d: %w", token, err)
	}
	return count > 0, nil
}

// Close gracefully closes the underlying database connection pool.
// It should be called during application shutdown after all active transactions complete.
func (db *DB) Close() error {
	if err := db.conn.Close(); err != nil {
		return fmt.Errorf("repository: Close database: %w", err)
	}
	log.Println("[repository] Database connection closed")
	return nil
}
