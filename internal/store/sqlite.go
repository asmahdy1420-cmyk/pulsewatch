package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"time"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/check"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/uptime"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registers "sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS results (
	id         INTEGER PRIMARY KEY,
	check_id   TEXT    NOT NULL,
	at_ms      INTEGER NOT NULL,
	status     TEXT    NOT NULL,
	latency_us INTEGER NOT NULL,
	message    TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS results_check_at ON results (check_id, at_ms);

CREATE TABLE IF NOT EXISTS incidents (
	id         INTEGER PRIMARY KEY,
	check_id   TEXT    NOT NULL,
	status     TEXT    NOT NULL,
	cause      TEXT    NOT NULL DEFAULT '',
	started_ms INTEGER NOT NULL,
	ended_ms   INTEGER
);
CREATE INDEX IF NOT EXISTS incidents_check_started ON incidents (check_id, started_ms);
`

const msPerDay = int64(24 * time.Hour / time.Millisecond)

// SQLite is a Store backed by a single SQLite database file.
type SQLite struct {
	db *sql.DB
}

var _ Store = (*SQLite)(nil)

// OpenSQLite opens (creating if needed) the database at path and applies
// the schema. WAL mode and a busy timeout keep the status page responsive
// while probes are being written.
func OpenSQLite(ctx context.Context, path string) (*SQLite, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite allows one writer at a time; a single connection avoids
	// SQLITE_BUSY churn and is plenty for this write rate.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &SQLite{db: db}, nil
}

func toMS(t time.Time) int64 { return t.UnixMilli() }

func fromMS(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// SaveResult implements Store.
func (s *SQLite) SaveResult(ctx context.Context, r check.Result) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO results (check_id, at_ms, status, latency_us, message) VALUES (?, ?, ?, ?, ?)`,
		r.CheckID, toMS(r.At), string(r.Status), r.Latency.Microseconds(), r.Message)
	return err
}

// History implements Store.
func (s *SQLite) History(ctx context.Context, checkID string, limit int) ([]check.Result, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT at_ms, status, latency_us, message FROM results
		 WHERE check_id = ? ORDER BY at_ms DESC, id DESC LIMIT ?`, checkID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []check.Result
	for rows.Next() {
		var at, lat int64
		var status, msg string
		if err := rows.Scan(&at, &status, &lat, &msg); err != nil {
			return nil, err
		}
		out = append(out, check.Result{
			CheckID: checkID,
			At:      fromMS(at),
			Status:  check.Status(status),
			Latency: time.Duration(lat) * time.Microsecond,
			Message: msg,
		})
	}
	return out, rows.Err()
}

const tallyCols = `
	COALESCE(SUM(status = 'up'), 0),
	COALESCE(SUM(status = 'degraded'), 0),
	COALESCE(SUM(status = 'down'), 0)`

// Tally implements Store.
func (s *SQLite) Tally(ctx context.Context, checkID string, from, to time.Time) (uptime.Tally, error) {
	var t uptime.Tally
	err := s.db.QueryRowContext(ctx,
		`SELECT`+tallyCols+` FROM results WHERE check_id = ? AND at_ms >= ? AND at_ms < ?`,
		checkID, toMS(from), toMS(to)).Scan(&t.Up, &t.Degraded, &t.Down)
	return t, err
}

// Daily implements Store.
func (s *SQLite) Daily(ctx context.Context, checkID string, from, to time.Time) ([]uptime.Day, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT at_ms / ? AS day,`+tallyCols+`
		 FROM results WHERE check_id = ? AND at_ms >= ? AND at_ms < ?
		 GROUP BY day ORDER BY day`,
		msPerDay, checkID, toMS(from), toMS(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uptime.Day
	for rows.Next() {
		var day int64
		var d uptime.Day
		if err := rows.Scan(&day, &d.Up, &d.Degraded, &d.Down); err != nil {
			return nil, err
		}
		d.Day = fromMS(day * msPerDay)
		out = append(out, d)
	}
	return out, rows.Err()
}

// OpenIncident implements Store.
func (s *SQLite) OpenIncident(ctx context.Context, inc *Incident) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO incidents (check_id, status, cause, started_ms) VALUES (?, ?, ?, ?)`,
		inc.CheckID, string(inc.Status), inc.Cause, toMS(inc.Started))
	if err != nil {
		return err
	}
	inc.ID, err = res.LastInsertId()
	return err
}

func (s *SQLite) execOne(ctx context.Context, q string, args ...any) error {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// EscalateIncident implements Store.
func (s *SQLite) EscalateIncident(ctx context.Context, id int64, st check.Status) error {
	var cur string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM incidents WHERE id = ?`, id).Scan(&cur)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if st.Severity() <= check.Status(cur).Severity() {
		return nil
	}
	return s.execOne(ctx, `UPDATE incidents SET status = ? WHERE id = ?`, string(st), id)
}

// CloseIncident implements Store.
func (s *SQLite) CloseIncident(ctx context.Context, id int64, ended time.Time) error {
	return s.execOne(ctx, `UPDATE incidents SET ended_ms = ? WHERE id = ?`, toMS(ended), id)
}

// ListIncidents implements Store.
func (s *SQLite) ListIncidents(ctx context.Context, f IncidentFilter) ([]Incident, error) {
	q := `SELECT id, check_id, status, cause, started_ms, ended_ms FROM incidents WHERE 1 = 1`
	var args []any
	if f.CheckID != "" {
		q += ` AND check_id = ?`
		args = append(args, f.CheckID)
	}
	if f.OpenOnly {
		q += ` AND ended_ms IS NULL`
	}
	q += ` ORDER BY started_ms DESC, id DESC`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		var inc Incident
		var status string
		var started int64
		var ended sql.NullInt64
		if err := rows.Scan(&inc.ID, &inc.CheckID, &status, &inc.Cause, &started, &ended); err != nil {
			return nil, err
		}
		inc.Status = check.Status(status)
		inc.Started = fromMS(started)
		if ended.Valid {
			e := fromMS(ended.Int64)
			inc.Ended = &e
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

// Prune implements Store.
func (s *SQLite) Prune(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM results WHERE at_ms < ?`, toMS(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Close implements Store.
func (s *SQLite) Close() error { return s.db.Close() }
