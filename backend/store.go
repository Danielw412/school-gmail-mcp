package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// Store owns the SQLite database. Nothing about the mailbox is cached in memory:
// every query goes to SQLite, whose page cache is capped per connection.
type Store struct {
	db   *sql.DB
	path string
}

const driverName = "sqlite3_school_email_sync"

var registerDriver sync.Once

// connectionPragmas run on every new connection. cache_size is negative KiB, so
// each connection keeps at most 2 MiB of pages; the pool holds at most 4.
var connectionPragmas = []string{
	"PRAGMA journal_mode = WAL",
	"PRAGMA synchronous = NORMAL",
	"PRAGMA foreign_keys = ON",
	"PRAGMA busy_timeout = 5000",
	"PRAGMA cache_size = -2000",
	"PRAGMA journal_size_limit = 67108864",
	"PRAGMA temp_store = FILE",
}

func openStore(path string) (*Store, error) {
	registerDriver.Do(func() {
		sql.Register(driverName, &sqlite3.SQLiteDriver{
			ConnectHook: func(c *sqlite3.SQLiteConn) error {
				for _, p := range connectionPragmas {
					if _, err := c.Exec(p, nil); err != nil {
						return fmt.Errorf("%s: %w", p, err)
					}
				}
				return nil
			},
		})
	})
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	// _txlock=immediate: every write transaction takes the write lock up front, so
	// concurrent writers queue on busy_timeout instead of failing on lock upgrade.
	db, err := sql.Open(driverName, "file:"+path+"?_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(2 * time.Minute)

	s := &Store{db: db, path: path}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) Close() error {
	// Let SQLite refresh its query planner statistics before exit; cheap when idle.
	_, _ = s.db.Exec("PRAGMA optimize")
	return s.db.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	var one int
	return s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

// Backup writes a consistent, compacted snapshot with VACUUM INTO. It is safe to
// run while the service is writing (WAL readers see a stable snapshot).
func (s *Store) Backup(ctx context.Context, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("%s already exists", dest)
	}
	if strings.ContainsRune(dest, 0) {
		return errors.New("invalid destination")
	}
	_, err := s.db.ExecContext(ctx, "VACUUM INTO ?", dest)
	return err
}

// migrations[i] upgrades the schema from user_version i to i+1.
var migrations = []string{schemaV1}

func (s *Store) migrate(ctx context.Context) error {
	var v int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d)", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("schema v%d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// All timestamps are Unix milliseconds. Gmail-side times come from Gmail
// (internal_date); *_at columns are this server's clock.
const schemaV1 = `
CREATE TABLE messages (
  gmail_id            TEXT PRIMARY KEY,
  thread_id           TEXT NOT NULL,
  history_id          TEXT NOT NULL DEFAULT '',
  rfc822_message_id   TEXT NOT NULL DEFAULT '',
  in_reply_to         TEXT NOT NULL DEFAULT '',
  references_hdr      TEXT NOT NULL DEFAULT '',
  from_addr           TEXT NOT NULL DEFAULT '',
  to_addrs            TEXT NOT NULL DEFAULT '',
  cc_addrs            TEXT NOT NULL DEFAULT '',
  bcc_addrs           TEXT NOT NULL DEFAULT '',
  reply_to            TEXT NOT NULL DEFAULT '',
  subject             TEXT NOT NULL DEFAULT '',
  date_header         TEXT NOT NULL DEFAULT '',
  internal_date       INTEGER NOT NULL,
  snippet             TEXT NOT NULL DEFAULT '',
  size_estimate       INTEGER NOT NULL DEFAULT 0,
  body_text           TEXT,
  body_text_truncated INTEGER NOT NULL DEFAULT 0,
  body_text_derived   INTEGER NOT NULL DEFAULT 0,  -- 1 = plain text generated from the HTML part
  body_html           TEXT,
  body_html_truncated INTEGER NOT NULL DEFAULT 0,
  label_ids           TEXT NOT NULL DEFAULT '[]',  -- sorted JSON array of Gmail label IDs
  is_unread           INTEGER NOT NULL DEFAULT 0,
  is_starred          INTEGER NOT NULL DEFAULT 0,
  is_important        INTEGER NOT NULL DEFAULT 0,
  in_inbox            INTEGER NOT NULL DEFAULT 0,
  in_sent             INTEGER NOT NULL DEFAULT 0,
  in_trash            INTEGER NOT NULL DEFAULT 0,
  in_spam             INTEGER NOT NULL DEFAULT 0,
  is_draft            INTEGER NOT NULL DEFAULT 0,
  gmail_deleted       INTEGER NOT NULL DEFAULT 0,  -- permanently deleted in Gmail; the row is kept
  first_synced_at     INTEGER NOT NULL,
  last_synced_at      INTEGER NOT NULL,
  state_changed_at    INTEGER NOT NULL
);
CREATE INDEX messages_by_date   ON messages(internal_date DESC, gmail_id DESC);
CREATE INDEX messages_by_thread ON messages(thread_id, internal_date);
CREATE INDEX messages_unread    ON messages(internal_date DESC) WHERE is_unread = 1;

-- Metadata only. Content is not downloaded; the fetch_* columns reserve space for
-- on-demand fetching later. Gmail attachment IDs can change between API reads, so
-- (gmail_id, part_id) is the stable key and attachment_id is refreshed on refetch.
CREATE TABLE attachments (
  id                  INTEGER PRIMARY KEY,
  gmail_id            TEXT NOT NULL REFERENCES messages(gmail_id) ON DELETE CASCADE,
  part_id             TEXT NOT NULL,
  filename            TEXT NOT NULL DEFAULT '',
  mime_type           TEXT NOT NULL DEFAULT '',
  size                INTEGER NOT NULL DEFAULT 0,
  gmail_attachment_id TEXT NOT NULL DEFAULT '',
  content_id          TEXT NOT NULL DEFAULT '',
  is_inline           INTEGER NOT NULL DEFAULT 0,
  fetch_state         TEXT NOT NULL DEFAULT 'none'
                      CHECK (fetch_state IN ('none', 'requested', 'stored', 'failed')),
  stored_path         TEXT,
  sha256              TEXT,
  fetched_at          INTEGER,
  UNIQUE (gmail_id, part_id)
);

-- External-content FTS5 index over messages; kept in sync by triggers.
CREATE VIRTUAL TABLE messages_fts USING fts5(
  subject, from_addr, to_addrs, cc_addrs, body_text,
  content = 'messages', content_rowid = 'rowid',
  tokenize = 'unicode61 remove_diacritics 2'
);
CREATE TRIGGER messages_fts_ai AFTER INSERT ON messages BEGIN
  INSERT INTO messages_fts(rowid, subject, from_addr, to_addrs, cc_addrs, body_text)
  VALUES (new.rowid, new.subject, new.from_addr, new.to_addrs, new.cc_addrs, new.body_text);
END;
CREATE TRIGGER messages_fts_ad AFTER DELETE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, subject, from_addr, to_addrs, cc_addrs, body_text)
  VALUES ('delete', old.rowid, old.subject, old.from_addr, old.to_addrs, old.cc_addrs, old.body_text);
END;
CREATE TRIGGER messages_fts_au AFTER UPDATE OF subject, from_addr, to_addrs, cc_addrs, body_text ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, subject, from_addr, to_addrs, cc_addrs, body_text)
  VALUES ('delete', old.rowid, old.subject, old.from_addr, old.to_addrs, old.cc_addrs, old.body_text);
  INSERT INTO messages_fts(rowid, subject, from_addr, to_addrs, cc_addrs, body_text)
  VALUES (new.rowid, new.subject, new.from_addr, new.to_addrs, new.cc_addrs, new.body_text);
END;

CREATE TABLE labels (
  label_id   TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  type       TEXT NOT NULL DEFAULT 'user',
  updated_at INTEGER NOT NULL
);

CREATE TABLE actions (
  id              TEXT PRIMARY KEY,
  type            TEXT NOT NULL CHECK (type IN ('send', 'reply')),
  status          TEXT NOT NULL CHECK (status IN ('pending', 'claimed', 'done', 'failed', 'cancelled')),
  payload         TEXT NOT NULL,
  idempotency_key TEXT UNIQUE,
  attempts        INTEGER NOT NULL DEFAULT 0,
  max_attempts    INTEGER NOT NULL,
  claim_id        TEXT,
  lease_until     INTEGER,
  not_before      INTEGER NOT NULL,
  last_error      TEXT,
  result          TEXT,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  finished_at     INTEGER
);
CREATE INDEX actions_queue   ON actions(status, not_before, created_at);
CREATE INDEX actions_created ON actions(created_at DESC);

CREATE TABLE kv (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
`

func (s *Store) setKV(ctx context.Context, key, value string, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO kv(key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, now)
	return err
}

func (s *Store) getKVTime(ctx context.Context, key string) (int64, error) {
	var t int64
	err := s.db.QueryRowContext(ctx, `SELECT updated_at FROM kv WHERE key = ?`, key).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return t, err
}
