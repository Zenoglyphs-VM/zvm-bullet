// Package store is the bot's SQLite database: who is connected to which
// address, and every game, seat and turn, which is what /leaderboard reads.
// The chain holds the money; this holds the story, and nothing in it is
// needed to pay anyone.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS users (
  tg_id     INTEGER PRIMARY KEY,
  name      TEXT NOT NULL,
  address   TEXT UNIQUE,
  link_tx   TEXT UNIQUE,
  linked_at INTEGER
);
CREATE TABLE IF NOT EXISTS connects (
  tg_id      INTEGER PRIMARY KEY,
  secret     INTEGER NOT NULL UNIQUE,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS games (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  chat_id     INTEGER NOT NULL,
  state       TEXT NOT NULL,   -- joining | starting | playing | ended | aborted | cancelled
  size        INTEGER NOT NULL,
  min_bet     TEXT NOT NULL,
  creator     INTEGER NOT NULL,
  created_at  INTEGER NOT NULL,
  started_at  INTEGER,
  ended_at    INTEGER,
  chamber     INTEGER,
  commit_hash TEXT,
  reveal      TEXT,            -- endGame's strings, newline-separated
  new_tx      TEXT,
  end_tx      TEXT,
  abort_tx    TEXT,
  loser_seat  INTEGER,
  note        TEXT
);
CREATE INDEX IF NOT EXISTS games_open ON games(state) WHERE state IN ('joining','starting','playing');
CREATE TABLE IF NOT EXISTS seats (
  game_id   INTEGER NOT NULL REFERENCES games(id),
  seat      INTEGER NOT NULL,
  tg_id     INTEGER NOT NULL,
  name      TEXT NOT NULL,
  address   TEXT NOT NULL,
  requested TEXT NOT NULL,
  bet       TEXT,              -- what the escrow took, after its cap
  winnings  TEXT,              -- on top of the returned stake; NULL for the loser
  PRIMARY KEY (game_id, seat)
);
CREATE TABLE IF NOT EXISTS turns (
  game_id INTEGER NOT NULL REFERENCES games(id),
  seq     INTEGER NOT NULL,
  seat    INTEGER NOT NULL,
  action  TEXT NOT NULL,       -- pull | pass
  auto    INTEGER NOT NULL,    -- 1 when the bot pulled for an AFK player
  died    INTEGER NOT NULL,
  at      INTEGER NOT NULL,
  PRIMARY KEY (game_id, seq)
);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// --- users ---

type User struct {
	TgID    int64
	Name    string
	Address string
}

var ErrNotFound = errors.New("not found")

func (s *Store) User(ctx context.Context, tgID int64) (*User, error) {
	var u User
	var addr sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT tg_id, name, address FROM users WHERE tg_id = ?`, tgID).Scan(&u.TgID, &u.Name, &addr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	u.Address = addr.String
	return &u, err
}

// SetConnect stores a pending secret for a user, replacing any earlier one.
// A secret already pending for somebody else is refused, so the caller draws
// another.
func (s *Store) SetConnect(ctx context.Context, tgID int64, secret uint32, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO connects(tg_id, secret, created_at) VALUES(?,?,?)
		ON CONFLICT(tg_id) DO UPDATE SET secret = excluded.secret, created_at = excluded.created_at`, tgID, int64(secret), now.Unix())
	return err
}

// PendingConnect returns the user's secret and when it was issued.
func (s *Store) PendingConnect(ctx context.Context, tgID int64) (uint32, time.Time, error) {
	var secret, at int64
	err := s.db.QueryRowContext(ctx, `SELECT secret, created_at FROM connects WHERE tg_id = ?`, tgID).Scan(&secret, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, time.Time{}, ErrNotFound
	}
	return uint32(secret), time.Unix(at, 0), err
}

var ErrTxUsed = errors.New("that transaction has already connected a wallet")

// Link binds an address to a user, from a connectAndApprove that carried the
// user's secret. The address moves if another user held it: the transaction
// proves whoever sent the secret controls it now. A transaction links once.
func (s *Store) Link(ctx context.Context, tgID int64, name, address, tx string, now time.Time) error {
	t, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer t.Rollback()
	var n int
	if err := t.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE link_tx = ?`, tx).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrTxUsed
	}
	if _, err := t.ExecContext(ctx, `UPDATE users SET address = NULL, link_tx = NULL, linked_at = NULL WHERE address = ? AND tg_id != ?`, address, tgID); err != nil {
		return err
	}
	if _, err := t.ExecContext(ctx, `INSERT INTO users(tg_id, name, address, link_tx, linked_at) VALUES(?,?,?,?,?)
		ON CONFLICT(tg_id) DO UPDATE SET name = excluded.name, address = excluded.address, link_tx = excluded.link_tx, linked_at = excluded.linked_at`,
		tgID, name, address, tx, now.Unix()); err != nil {
		return err
	}
	if _, err := t.ExecContext(ctx, `DELETE FROM connects WHERE tg_id = ?`, tgID); err != nil {
		return err
	}
	return t.Commit()
}

// --- games ---

type Seat struct {
	Seat      int
	TgID      int64
	Name      string
	Address   string
	Requested *big.Int
}

func (s *Store) NewGame(ctx context.Context, chat int64, size int, minBet *big.Int, creator int64, now time.Time) (int64, error) {
	r, err := s.db.ExecContext(ctx, `INSERT INTO games(chat_id, state, size, min_bet, creator, created_at) VALUES(?, 'joining', ?, ?, ?, ?)`,
		chat, size, minBet.String(), creator, now.Unix())
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

func (s *Store) AddSeat(ctx context.Context, game int64, st Seat) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO seats(game_id, seat, tg_id, name, address, requested) VALUES(?,?,?,?,?,?)`,
		game, st.Seat, st.TgID, st.Name, st.Address, st.Requested.String())
	return err
}

func (s *Store) SetState(ctx context.Context, game int64, state, note string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE games SET state = ?, note = COALESCE(NULLIF(?, ''), note) WHERE id = ?`, state, note, game)
	return err
}

// Starting records the commitment before newGame is sent, so a crash while it
// is in flight leaves a row saying so.
func (s *Store) Starting(ctx context.Context, game int64, chamber int, commit, reveal string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE games SET state = 'starting', chamber = ?, commit_hash = ?, reveal = ? WHERE id = ?`,
		chamber, commit, reveal, game)
	return err
}

func (s *Store) Started(ctx context.Context, game int64, tx string, bets []*big.Int, now time.Time) error {
	t, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer t.Rollback()
	if _, err := t.ExecContext(ctx, `UPDATE games SET state = 'playing', new_tx = ?, started_at = ? WHERE id = ?`, tx, now.Unix(), game); err != nil {
		return err
	}
	for i, b := range bets {
		if _, err := t.ExecContext(ctx, `UPDATE seats SET bet = ? WHERE game_id = ? AND seat = ?`, b.String(), game, i); err != nil {
			return err
		}
	}
	return t.Commit()
}

func (s *Store) Turn(ctx context.Context, game int64, seq, seat int, action string, auto, died bool, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO turns(game_id, seq, seat, action, auto, died, at) VALUES(?,?,?,?,?,?,?)`,
		game, seq, seat, action, b2i(auto), b2i(died), now.Unix())
	return err
}

func (s *Store) Ended(ctx context.Context, game int64, loser int, tx string, winnings map[int]*big.Int, now time.Time) error {
	t, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer t.Rollback()
	if _, err := t.ExecContext(ctx, `UPDATE games SET state = 'ended', end_tx = ?, loser_seat = ?, ended_at = ? WHERE id = ?`, tx, loser, now.Unix(), game); err != nil {
		return err
	}
	for seat, w := range winnings {
		if _, err := t.ExecContext(ctx, `UPDATE seats SET winnings = ? WHERE game_id = ? AND seat = ?`, w.String(), game, seat); err != nil {
			return err
		}
	}
	return t.Commit()
}

func (s *Store) Aborted(ctx context.Context, game int64, tx, note string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE games SET state = 'aborted', abort_tx = ?, ended_at = ?, note = ? WHERE id = ?`, tx, now.Unix(), note, game)
	return err
}

func (s *Store) Cancelled(ctx context.Context, game int64, note string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE games SET state = 'cancelled', ended_at = ?, note = ? WHERE id = ?`, now.Unix(), note, game)
	return err
}

// Unfinished is a game the bot was running when it stopped.
type Unfinished struct {
	ID    int64
	Chat  int64
	State string
}

func (s *Store) Unfinished(ctx context.Context) ([]Unfinished, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, chat_id, state FROM games WHERE state IN ('joining','starting','playing') ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Unfinished
	for rows.Next() {
		var u Unfinished
		if err := rows.Scan(&u.ID, &u.Chat, &u.State); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// --- leaderboard ---

type Standing struct {
	TgID   int64
	Name   string
	Games  int
	Deaths int
	Net    *big.Int // winnings minus stakes lost, in base units
}

// Leaderboard ranks players over every game that paid out, by net result.
func (s *Store) Leaderboard(ctx context.Context, chat int64, limit int) ([]Standing, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.tg_id, s.name, s.seat = g.loser_seat, s.bet, COALESCE(s.winnings, '0')
		FROM seats s JOIN games g ON g.id = s.game_id
		WHERE g.state = 'ended' AND g.chat_id = ?
		ORDER BY g.id`, chat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	by := map[int64]*Standing{}
	for rows.Next() {
		var id int64
		var name, bet, win string
		var died bool
		if err := rows.Scan(&id, &name, &died, &bet, &win); err != nil {
			return nil, err
		}
		st := by[id]
		if st == nil {
			st = &Standing{TgID: id, Net: new(big.Int)}
			by[id] = st
		}
		st.Name = name
		st.Games++
		if died {
			st.Deaths++
			b, _ := new(big.Int).SetString(bet, 10)
			st.Net.Sub(st.Net, b)
		} else {
			w, _ := new(big.Int).SetString(win, 10)
			st.Net.Add(st.Net, w)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Standing, 0, len(by))
	for _, st := range by {
		out = append(out, *st)
	}
	sortStandings(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func sortStandings(s []Standing) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && less(s[j], s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func less(a, b Standing) bool {
	if c := a.Net.Cmp(b.Net); c != 0 {
		return c > 0
	}
	if a.Games != b.Games {
		return a.Games > b.Games
	}
	return a.TgID < b.TgID
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Game is one row, for /verify.
type Game struct {
	ID      int64
	Chat    int64
	State   string
	Size    int
	Commit  string
	Reveal  string
	NewTx   string
	EndTx   string
	Chamber sql.NullInt64
}

func (s *Store) Game(ctx context.Context, id int64) (*Game, error) {
	var g Game
	var commit, reveal, newTx, endTx sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id, chat_id, state, size, commit_hash, reveal, new_tx, end_tx, chamber FROM games WHERE id = ?`, id).
		Scan(&g.ID, &g.Chat, &g.State, &g.Size, &commit, &reveal, &newTx, &endTx, &g.Chamber)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("game %d: %w", id, err)
	}
	g.Commit, g.Reveal, g.NewTx, g.EndTx = commit.String, reveal.String, newTx.String, endTx.String
	return &g, nil
}
