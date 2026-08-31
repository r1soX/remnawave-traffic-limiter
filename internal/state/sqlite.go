package state

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	StateActive  = "ACTIVE"
	StateBlocked = "BLOCKED"
)

type Store struct {
	mu sync.Mutex
	db *sql.DB
}

type UserState struct {
	UserUUID           string
	OriginalLimitBytes int64
	OriginalStrategy   string
	WhitelistState     string
	BlockedAt          string
	LastEvent          string
	LastEventAt        string
}

// PairedUser records the two Remnawave identities that represent one customer
// subscription. Main is deliberately unlimited; only WhiteList carries quota.
type PairedUser struct {
	MainUserID         int64
	MainShortUUID      string
	WhiteUserID        int64
	WhiteShortUUID     string
	QuotaBytes         int64
	TrafficStrategy    string
	ExpireAt           string
	LastSourceLimit    int64
	LastSourceExpireAt string
	State              string
	Enabled            bool
	CreatedAt          string
	UpdatedAt          string
}

func NewSQLite(path string) (*Store, error) {
	if path == "" {
		path = "/data/state.sqlite"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if err = db.Ping(); err != nil {
		return nil, err
	}
	if _, err = db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		return nil, err
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS user_state (
		user_uuid TEXT PRIMARY KEY,
		original_traffic_limit INTEGER,
		original_traffic_strategy TEXT,
		whitelist_state TEXT,
		blocked_at TEXT,
		last_event TEXT,
		last_event_at TEXT
	);`); err != nil {
		return nil, err
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS paired_users (
		main_user_id INTEGER PRIMARY KEY,
		main_short_uuid TEXT NOT NULL,
		white_user_id INTEGER NOT NULL UNIQUE,
		white_short_uuid TEXT NOT NULL,
		quota_bytes INTEGER NOT NULL,
		traffic_strategy TEXT NOT NULL,
		expire_at TEXT NOT NULL,
		last_source_limit INTEGER NOT NULL,
		last_source_expire_at TEXT NOT NULL,
		state TEXT NOT NULL,
		paired_enabled INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);`); err != nil {
		return nil, err
	}
	if err := ensurePairedEnabledColumn(db); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func ensurePairedEnabledColumn(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(paired_users)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == "paired_enabled" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(`ALTER TABLE paired_users ADD COLUMN paired_enabled INTEGER NOT NULL DEFAULT 1`)
	return err
}

func (s *Store) UpsertPair(pair PairedUser) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store is nil")
	}
	if pair.MainUserID <= 0 || pair.WhiteUserID <= 0 {
		return fmt.Errorf("main and white user ids are required")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if pair.CreatedAt == "" {
		pair.CreatedAt = now
	}
	pair.UpdatedAt = now
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
		INSERT INTO paired_users (
			main_user_id, main_short_uuid, white_user_id, white_short_uuid,
			quota_bytes, traffic_strategy, expire_at, last_source_limit,
			last_source_expire_at, state, paired_enabled, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(main_user_id) DO UPDATE SET
			main_short_uuid=excluded.main_short_uuid,
			white_user_id=excluded.white_user_id,
			white_short_uuid=excluded.white_short_uuid,
			quota_bytes=excluded.quota_bytes,
			traffic_strategy=excluded.traffic_strategy,
			expire_at=excluded.expire_at,
			last_source_limit=excluded.last_source_limit,
			last_source_expire_at=excluded.last_source_expire_at,
			state=excluded.state,
			paired_enabled=excluded.paired_enabled,
			updated_at=excluded.updated_at
	`, pair.MainUserID, pair.MainShortUUID, pair.WhiteUserID, pair.WhiteShortUUID,
		pair.QuotaBytes, pair.TrafficStrategy, pair.ExpireAt, pair.LastSourceLimit,
		pair.LastSourceExpireAt, pair.State, pair.Enabled, pair.CreatedAt, pair.UpdatedAt)
	return err
}

func (s *Store) GetPairByMainUserID(mainUserID int64) (*PairedUser, error) {
	return s.getPair(`main_user_id = ?`, mainUserID)
}

func (s *Store) GetPairByWhiteUserID(whiteUserID int64) (*PairedUser, error) {
	return s.getPair(`white_user_id = ?`, whiteUserID)
}

func (s *Store) getPair(where string, value int64) (*PairedUser, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	query := `SELECT main_user_id, main_short_uuid, white_user_id, white_short_uuid,
		quota_bytes, traffic_strategy, expire_at, last_source_limit,
		last_source_expire_at, state, paired_enabled, created_at, updated_at FROM paired_users WHERE ` + where
	var pair PairedUser
	if err := s.db.QueryRow(query, value).Scan(
		&pair.MainUserID, &pair.MainShortUUID, &pair.WhiteUserID, &pair.WhiteShortUUID,
		&pair.QuotaBytes, &pair.TrafficStrategy, &pair.ExpireAt, &pair.LastSourceLimit,
		&pair.LastSourceExpireAt, &pair.State, &pair.Enabled, &pair.CreatedAt, &pair.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &pair, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) UpsertUserState(state UserState) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
		INSERT INTO user_state (
			user_uuid, original_traffic_limit, original_traffic_strategy, whitelist_state,
			blocked_at, last_event, last_event_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_uuid) DO UPDATE SET
			original_traffic_limit=excluded.original_traffic_limit,
			original_traffic_strategy=excluded.original_traffic_strategy,
			whitelist_state=excluded.whitelist_state,
			blocked_at=excluded.blocked_at,
			last_event=excluded.last_event,
			last_event_at=excluded.last_event_at
	`, state.UserUUID, state.OriginalLimitBytes, state.OriginalStrategy, state.WhitelistState, state.BlockedAt, state.LastEvent, state.LastEventAt)
	return err
}

func (s *Store) GetUserState(userUUID string) (*UserState, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(`SELECT user_uuid, original_traffic_limit, original_traffic_strategy, whitelist_state, blocked_at, last_event, last_event_at FROM user_state WHERE user_uuid = ?`, userUUID)
	var state UserState
	if err := row.Scan(&state.UserUUID, &state.OriginalLimitBytes, &state.OriginalStrategy, &state.WhitelistState, &state.BlockedAt, &state.LastEvent, &state.LastEventAt); err != nil {
		return nil, err
	}
	return &state, nil
}

func (s *Store) MarkEvent(userUUID, event string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store is nil")
	}
	stateRec, err := s.GetUserState(userUUID)
	if err != nil {
		stateRec = &UserState{UserUUID: userUUID}
	}
	stateRec.LastEvent = event
	stateRec.LastEventAt = time.Now().UTC().Format(time.RFC3339)
	return s.UpsertUserState(*stateRec)
}

func (s *Store) SetBlocked(userUUID string) error {
	stateRec, err := s.GetUserState(userUUID)
	if err != nil {
		stateRec = &UserState{UserUUID: userUUID}
	}
	stateRec.WhitelistState = StateBlocked
	stateRec.BlockedAt = time.Now().UTC().Format(time.RFC3339)
	return s.UpsertUserState(*stateRec)
}

func (s *Store) SetActive(userUUID string) error {
	stateRec, err := s.GetUserState(userUUID)
	if err != nil {
		stateRec = &UserState{UserUUID: userUUID}
	}
	stateRec.WhitelistState = StateActive
	stateRec.BlockedAt = ""
	return s.UpsertUserState(*stateRec)
}

func (s *Store) LastEventFor(userUUID string) (string, error) {
	stateRec, err := s.GetUserState(userUUID)
	if err != nil {
		return "", err
	}
	return stateRec.LastEvent, nil
}
