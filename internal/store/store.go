// Package store persists the director's state in SQLite.
package store

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/tiny-systems/boardingparty/internal/project"
)

//go:embed schema.sql
var schema string

var ErrNotFound = errors.New("not found")

type RunState string // "pending","running","done","stuck","failed"

const (
	RunPending RunState = "pending"
	RunRunning RunState = "running"
	RunDone    RunState = "done"
	RunStuck   RunState = "stuck"
	RunFailed  RunState = "failed"
)

type Run struct {
	ID       string    `json:"id"`
	Token    string    `json:"token"`
	Persona  string    `json:"persona"`
	Role     string    `json:"role"`
	Story    string    `json:"story"`
	State    RunState  `json:"state"`
	Outcome  string    `json:"outcome"` // agent's summary or failure reason
	Session  string    `json:"session"` // tiny Session name
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
}

type Bug struct {
	ID       string    `json:"id"`
	RunID    string    `json:"run_id"`
	Summary  string    `json:"summary"`
	Step     string    `json:"step"`
	Evidence string    `json:"evidence"`
	Severity string    `json:"severity"`
	Note     string    `json:"note"`
	Created  time.Time `json:"created"`
}

type Store struct{ db *sql.DB }

// Open opens (creating if needed) the database at path and applies the schema.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One writer connection avoids SQLITE_BUSY between our own goroutines.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) PutProject(p project.Project) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO project(id, body) VALUES(1, ?)
		ON CONFLICT(id) DO UPDATE SET body = excluded.body`, string(b))
	return err
}

func (s *Store) Project() (project.Project, error) {
	var body string
	err := s.db.QueryRow(`SELECT body FROM project WHERE id = 1`).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return project.Project{}, ErrNotFound
	}
	if err != nil {
		return project.Project{}, err
	}
	var p project.Project
	err = json.Unmarshal([]byte(body), &p)
	return p, err
}

// PersonaState returns nil if the persona has no saved state.
func (s *Store) PersonaState(persona string) (json.RawMessage, error) {
	var st string
	err := s.db.QueryRow(`SELECT state FROM persona_state WHERE persona = ?`, persona).Scan(&st)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(st), nil
}

func (s *Store) SavePersonaState(persona string, state json.RawMessage) error {
	_, err := s.db.Exec(`INSERT INTO persona_state(persona, state) VALUES(?, ?)
		ON CONFLICT(persona) DO UPDATE SET state = excluded.state`, persona, string(state))
	return err
}

func ts(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromTS(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

const runCols = `id, token, persona, role, story, state, outcome, session, started, finished`

func scanRun(sc interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var st string
	var started, finished int64
	if err := sc.Scan(&r.ID, &r.Token, &r.Persona, &r.Role, &r.Story, &st, &r.Outcome, &r.Session, &started, &finished); err != nil {
		return Run{}, err
	}
	r.State = RunState(st)
	r.Started, r.Finished = fromTS(started), fromTS(finished)
	return r, nil
}

func (s *Store) CreateRun(r Run) error {
	_, err := s.db.Exec(`INSERT INTO runs(`+runCols+`) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Token, r.Persona, r.Role, r.Story, string(r.State), r.Outcome, r.Session, ts(r.Started), ts(r.Finished))
	return err
}

func (s *Store) RunByToken(token string) (Run, error) {
	r, err := scanRun(s.db.QueryRow(`SELECT `+runCols+` FROM runs WHERE token = ?`, token))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

func (s *Store) RunByID(id string) (Run, error) {
	r, err := scanRun(s.db.QueryRow(`SELECT `+runCols+` FROM runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

func (s *Store) UpdateRun(r Run) error {
	res, err := s.db.Exec(`UPDATE runs SET token=?, persona=?, role=?, story=?, state=?, outcome=?, session=?, started=?, finished=? WHERE id=?`,
		r.Token, r.Persona, r.Role, r.Story, string(r.State), r.Outcome, r.Session, ts(r.Started), ts(r.Finished), r.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// FinishRun atomically moves a pending or running run to a final state.
// It returns false if the run was not active (already finished or unknown).
func (s *Store) FinishRun(id string, state RunState, outcome string, finished time.Time) (bool, error) {
	res, err := s.db.Exec(`UPDATE runs SET state=?, outcome=?, finished=? WHERE id=? AND state IN ('pending','running')`,
		string(state), outcome, ts(finished), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) queryRuns(q string, args ...any) ([]Run, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Runs returns the newest runs first.
func (s *Store) Runs(limit int) ([]Run, error) {
	return s.queryRuns(`SELECT `+runCols+` FROM runs ORDER BY started DESC, rowid DESC LIMIT ?`, limit)
}

// ActiveRuns returns runs that are pending or running.
func (s *Store) ActiveRuns() ([]Run, error) {
	return s.queryRuns(`SELECT ` + runCols + ` FROM runs WHERE state IN ('pending','running') ORDER BY started`)
}

func (s *Store) AddBug(b Bug) error {
	_, err := s.db.Exec(`INSERT INTO bugs(id, run_id, summary, step, evidence, severity, note, created) VALUES(?,?,?,?,?,?,?,?)`,
		b.ID, b.RunID, b.Summary, b.Step, b.Evidence, b.Severity, b.Note, ts(b.Created))
	return err
}

func (s *Store) Bugs(runID string) ([]Bug, error) {
	rows, err := s.db.Query(`SELECT id, run_id, summary, step, evidence, severity, note, created FROM bugs WHERE run_id = ? ORDER BY created, rowid`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bug
	for rows.Next() {
		var b Bug
		var c int64
		if err := rows.Scan(&b.ID, &b.RunID, &b.Summary, &b.Step, &b.Evidence, &b.Severity, &b.Note, &c); err != nil {
			return nil, err
		}
		b.Created = fromTS(c)
		out = append(out, b)
	}
	return out, rows.Err()
}
