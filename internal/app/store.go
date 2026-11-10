package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"time"
)

type Decision struct {
	Category    string   `json:"category"`
	Confidence  float64  `json:"confidence"`
	Reason      string   `json:"reason"`
	Evidence    []string `json:"evidence"`
	Protected   bool     `json:"protected"`
	NeedsReview bool     `json:"needs_review"`
}
type Message struct {
	ID            string    `json:"id"`
	ThreadID      string    `json:"thread_id"`
	From          string    `json:"from"`
	Subject       string    `json:"subject"`
	Date          string    `json:"date"`
	Body          string    `json:"body,omitempty"`
	Labels        []string  `json:"labels"`
	Attachments   bool      `json:"attachments"`
	Truncated     bool      `json:"truncated"`
	Decision      *Decision `json:"decision,omitempty"`
	State         string    `json:"state"`
	Error         string    `json:"error,omitempty"`
	PendingAction string    `json:"pending_action,omitempty"`
	Attempts      int       `json:"attempts"`
	NextTry       int64     `json:"next_try"`
	Model         string    `json:"model,omitempty"`
	Updated       string    `json:"updated"`
}
type Event struct {
	ID        int64  `json:"id"`
	Time      string `json:"time"`
	MessageID string `json:"message_id"`
	Action    string `json:"action"`
	Detail    string `json:"detail"`
}
type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS messages(id TEXT PRIMARY KEY, state TEXT NOT NULL, data TEXT NOT NULL, updated TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY,value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS events(id INTEGER PRIMARY KEY AUTOINCREMENT,time TEXT NOT NULL,message_id TEXT NOT NULL,action TEXT NOT NULL,detail TEXT NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0600)
	return &Store{db}, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Put(m Message) error {
	m.Updated = time.Now().UTC().Format(time.RFC3339)
	b, e := json.Marshal(m)
	if e != nil {
		return e
	}
	_, e = s.db.Exec(`INSERT INTO messages VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET state=excluded.state,data=excluded.data,updated=excluded.updated`, m.ID, m.State, string(b), m.Updated)
	return e
}
func (s *Store) Get(id string) (Message, error) {
	var b string
	var m Message
	e := s.db.QueryRow(`SELECT data FROM messages WHERE id=?`, id).Scan(&b)
	if e != nil {
		return m, e
	}
	e = json.Unmarshal([]byte(b), &m)
	return m, e
}
func (s *Store) List(state string) ([]Message, error) {
	q := `SELECT data FROM messages`
	var args []any
	if state != "" {
		q += ` WHERE state=?`
		args = append(args, state)
	}
	q += ` ORDER BY updated DESC`
	rows, e := s.db.Query(q, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var b string
		var m Message
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(b), &m); e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *Store) Meta(k string) string {
	var v string
	_ = s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, k).Scan(&v)
	return v
}
func (s *Store) SetMeta(k, v string) error {
	_, e := s.db.Exec(`INSERT INTO meta VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v)
	return e
}
func (s *Store) Log(id, action, detail string) error {
	_, e := s.db.Exec(`INSERT INTO events(time,message_id,action,detail) VALUES(?,?,?,?)`, time.Now().UTC().Format(time.RFC3339), id, action, detail)
	return e
}
func (s *Store) Events() ([]Event, error) {
	rows, e := s.db.Query(`SELECT id,time,message_id,action,detail FROM events ORDER BY id DESC LIMIT 100`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var x Event
		if e = rows.Scan(&x.ID, &x.Time, &x.MessageID, &x.Action, &x.Detail); e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (s *Store) BindAccount(email string) error {
	old := s.Meta("account")
	if old != "" && old != email {
		return fmt.Errorf("此数据目录已经属于 %s，请为新邮箱使用新的数据目录", old)
	}
	return s.SetMeta("account", email)
}
