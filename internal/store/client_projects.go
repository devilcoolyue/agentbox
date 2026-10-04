package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const ClientProjectLimit = 128
const ClientTerminalLimit = 32

var (
	ErrClientMissing  = errors.New("project, terminal or workspace not found")
	ErrClientConflict = errors.New("project changed, path already mapped, or terminals still exist")
	ErrClientLimit    = errors.New("desktop resource limit reached")
	ErrClientInvalid  = errors.New("invalid desktop resource")
)

type ClientProject struct {
	ID        string   `json:"id"`
	SessionID string   `json:"session_id"`
	Name      string   `json:"name"`
	Path      string   `json:"path"`
	Arguments []string `json:"arguments"`
	Revision  int64    `json:"revision"`
	CreatedAt string   `json:"created_at"`
}

type ClientTerminal struct {
	ID        string   `json:"id"`
	SessionID string   `json:"session_id"`
	ProjectID string   `json:"project_id"`
	Kind      string   `json:"kind"`
	State     string   `json:"state"`
	Arguments []string `json:"arguments"`
	CreatedAt string   `json:"created_at"`
}

// ClientProjectPath is a canonical POSIX path relative to /workspace. Names are
// preserved; do not silently clean traversal, trailing slashes or backslashes.
func ClientProjectPath(value string) bool {
	return value != "" && len(value) <= 1024 && utf8.ValidString(value) && !strings.ContainsAny(value, "\\\x00\r\n") && !strings.HasPrefix(value, "/") && value != ".." && !strings.HasPrefix(value, "../") && path.Clean(value) == value
}

func validateClientProject(p ClientProject) error {
	if strings.TrimSpace(p.Name) == "" || !utf8.ValidString(p.Name) || len(p.Name) > 128 || strings.ContainsFunc(p.Name, unicode.IsControl) || !ClientProjectPath(p.Path) || len(p.Arguments) > 32 {
		return ErrClientInvalid
	}
	total := 0
	for _, arg := range p.Arguments {
		total += len(arg)
		if !utf8.ValidString(arg) || strings.ContainsRune(arg, 0) || len(arg) > 2048 {
			return ErrClientInvalid
		}
	}
	if total > 8192 {
		return ErrClientInvalid
	}
	return nil
}

type clientScanner interface{ Scan(...any) error }

func scanClientProject(row clientScanner) (ClientProject, error) {
	var p ClientProject
	var args string
	err := row.Scan(&p.ID, &p.SessionID, &p.Name, &p.Path, &args, &p.Revision, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrClientMissing
	}
	if err == nil {
		err = json.Unmarshal([]byte(args), &p.Arguments)
	}
	if p.Arguments == nil {
		p.Arguments = []string{}
	}
	return p, err
}

const clientProjectColumns = "id,session_id,name,path,arguments,revision,created_at"

func (s *Store) ClientProjects(session string) ([]ClientProject, error) {
	rows, err := s.db.Query("SELECT "+clientProjectColumns+" FROM client_projects WHERE session_id=? ORDER BY created_at,id", session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ClientProject{}
	for rows.Next() {
		p, err := scanClientProject(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}
func (s *Store) ClientProject(session, id string) (ClientProject, error) {
	return scanClientProject(s.db.QueryRow("SELECT "+clientProjectColumns+" FROM client_projects WHERE session_id=? AND id=?", session, id))
}

func (s *Store) CreateClientProject(p ClientProject) (ClientProject, error) {
	if err := validateClientProject(p); err != nil {
		return p, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	var present, count, duplicate int
	if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM sessions WHERE id=?),(SELECT COUNT(*) FROM client_projects WHERE session_id=?),EXISTS(SELECT 1 FROM client_projects WHERE session_id=? AND path=?)", p.SessionID, p.SessionID, p.SessionID, p.Path).Scan(&present, &count, &duplicate); err != nil {
		return p, err
	}
	if present == 0 {
		return p, ErrClientMissing
	}
	if duplicate != 0 {
		return p, ErrClientConflict
	}
	if count >= ClientProjectLimit {
		return p, ErrClientLimit
	}
	p.ID = NewID()
	p.Revision = 1
	p.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if p.Arguments == nil {
		p.Arguments = []string{}
	}
	args, _ := json.Marshal(p.Arguments)
	_, err = tx.Exec("INSERT INTO client_projects ("+clientProjectColumns+") VALUES (?,?,?,?,?,?,?)", p.ID, p.SessionID, p.Name, p.Path, string(args), p.Revision, p.CreatedAt)
	if err != nil {
		return p, err
	}
	return p, tx.Commit()
}

func (s *Store) UpdateClientProject(p ClientProject) (ClientProject, error) {
	if err := validateClientProject(p); err != nil {
		return p, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	old, err := scanClientProject(tx.QueryRow("SELECT "+clientProjectColumns+" FROM client_projects WHERE session_id=? AND id=?", p.SessionID, p.ID))
	if err != nil {
		return p, err
	}
	if old.Revision != p.Revision {
		return p, ErrClientConflict
	}
	if p.Path != old.Path {
		var count int
		if err = tx.QueryRow("SELECT (SELECT COUNT(*) FROM client_terminals WHERE project_id=?)+(SELECT COUNT(*) FROM client_projects WHERE session_id=? AND path=?)", p.ID, p.SessionID, p.Path).Scan(&count); err != nil {
			return p, err
		}
		if count != 0 {
			return p, ErrClientConflict
		}
	}
	if p.Arguments == nil {
		p.Arguments = []string{}
	}
	args, _ := json.Marshal(p.Arguments)
	p.Revision++
	p.CreatedAt = old.CreatedAt
	_, err = tx.Exec("UPDATE client_projects SET name=?,path=?,arguments=?,revision=? WHERE id=? AND session_id=?", p.Name, p.Path, string(args), p.Revision, p.ID, p.SessionID)
	if err != nil {
		return p, err
	}
	return p, tx.Commit()
}

func (s *Store) DeleteClientProject(session, id string, revision int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	old, err := scanClientProject(tx.QueryRow("SELECT "+clientProjectColumns+" FROM client_projects WHERE session_id=? AND id=?", session, id))
	if err != nil {
		return err
	}
	if old.Revision != revision {
		return ErrClientConflict
	}
	var count int
	if err = tx.QueryRow("SELECT COUNT(*) FROM client_terminals WHERE project_id=?", id).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return ErrClientConflict
	}
	if _, err = tx.Exec("DELETE FROM client_projects WHERE session_id=? AND id=?", session, id); err != nil {
		return err
	}
	return tx.Commit()
}

func scanClientTerminal(row clientScanner) (ClientTerminal, error) {
	var t ClientTerminal
	var args string
	err := row.Scan(&t.ID, &t.SessionID, &t.ProjectID, &t.Kind, &t.State, &args, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrClientMissing
	}
	if err == nil {
		err = json.Unmarshal([]byte(args), &t.Arguments)
	}
	if t.Arguments == nil {
		t.Arguments = []string{}
	}
	return t, err
}

const clientTerminalColumns = "id,session_id,project_id,kind,state,arguments,created_at"

func (s *Store) ClientTerminals(session string) ([]ClientTerminal, error) {
	rows, err := s.db.Query("SELECT "+clientTerminalColumns+" FROM client_terminals WHERE session_id=? ORDER BY created_at,id", session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ClientTerminal{}
	for rows.Next() {
		t, err := scanClientTerminal(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, t)
	}
	return result, rows.Err()
}
func (s *Store) ClientTerminal(session, id string) (ClientTerminal, error) {
	return scanClientTerminal(s.db.QueryRow("SELECT "+clientTerminalColumns+" FROM client_terminals WHERE session_id=? AND id=?", session, id))
}
func (s *Store) CreateClientTerminal(session, project, kind string) (ClientTerminal, error) {
	var terminal ClientTerminal
	if kind != "shell" && kind != "agent" {
		return terminal, ErrClientInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return terminal, err
	}
	defer tx.Rollback()
	p, err := scanClientProject(tx.QueryRow("SELECT "+clientProjectColumns+" FROM client_projects WHERE session_id=? AND id=?", session, project))
	if err != nil {
		return terminal, err
	}
	var count int
	if err = tx.QueryRow("SELECT COUNT(*) FROM client_terminals WHERE session_id=?", session).Scan(&count); err != nil {
		return terminal, err
	}
	if count >= ClientTerminalLimit {
		return terminal, ErrClientLimit
	}
	args := []string{}
	if kind == "agent" {
		args = p.Arguments
	}
	terminal = ClientTerminal{ID: NewID(), SessionID: session, ProjectID: project, Kind: kind, State: "open", Arguments: args, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	encoded, _ := json.Marshal(args)
	_, err = tx.Exec("INSERT INTO client_terminals ("+clientTerminalColumns+") VALUES (?,?,?,?,?,?,?)", terminal.ID, session, project, kind, terminal.State, string(encoded), terminal.CreatedAt)
	if err != nil {
		return terminal, err
	}
	return terminal, tx.Commit()
}

// Closing is persisted before Docker teardown. Failed teardown remains visible
// and retryable; no late connection can recreate a terminal being terminated.
func (s *Store) BeginCloseClientTerminal(session, id string) error {
	result, err := s.db.Exec("UPDATE client_terminals SET state='closing' WHERE session_id=? AND id=?", session, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrClientMissing
	}
	return nil
}
func (s *Store) DeleteClientTerminal(session, id string) error {
	_, err := s.db.Exec("DELETE FROM client_terminals WHERE session_id=? AND id=? AND state='closing'", session, id)
	return err
}

// IDs are never provided by clients, but validate before using persistent IDs
// as tmux socket names in case an imported database is malformed.
func ValidClientResourceID(id string) bool {
	if len(id) != 12 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'f' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
