package store

import (
	"database/sql"
	"errors"
	"time"
)

// GitProfile is the user's commit identity. Credentials for remotes are kept
// separate and will be added by the Git connection work; this profile only
// controls the author/committer fields of web-created local commits.
type GitProfile struct {
	User      string    `json:"user"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	UpdatedAt time.Time `json:"updated_at"`
}

func defaultGitProfile(user string) GitProfile {
	return GitProfile{User: user, Name: user, Email: user + "@localhost"}
}

// GetGitProfile returns a usable identity even before the user has explicitly
// configured one. Defaults are deliberately deterministic and are not written
// until the user saves them.
func (s *Store) GetGitProfile(user string) (GitProfile, error) {
	p := defaultGitProfile(user)
	var updated string
	err := s.db.QueryRow("SELECT name, email, updated_at FROM git_profiles WHERE user = ?", user).
		Scan(&p.Name, &p.Email, &updated)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return GitProfile{}, err
	}
	if err == nil {
		p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	}
	if p.Name == "" {
		p.Name = user
	}
	if p.Email == "" {
		p.Email = user + "@localhost"
	}
	return p, nil
}

func (s *Store) SetGitProfile(user, name, email string) (GitProfile, error) {
	now := time.Now()
	_, err := s.db.Exec(`INSERT INTO git_profiles (user, name, email, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(user) DO UPDATE SET name=excluded.name, email=excluded.email, updated_at=excluded.updated_at`,
		user, name, email, now.Format(time.RFC3339Nano))
	if err != nil {
		return GitProfile{}, err
	}
	return GitProfile{User: user, Name: name, Email: email, UpdatedAt: now}, nil
}
