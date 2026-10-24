// Package announce stores the single site-wide announcement shown as a banner
// to every signed-in user: a plain notice, or a scheduled release with a
// countdown that turns into "upgraded to vX" once the new server starts.
//
// The announcement lives in <data_dir>/announcement.json, not in SQLite: one
// record does not justify a schema migration that would block rolling back to
// an older binary. The server and the `agentbox announce` command both write
// the file; the server notices outside writes by polling Revision. The file is
// left out of system backups on purpose: restoring a backup must not bring back
// a stale "upgrading at 23:00" banner.
package announce

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"agentbox/internal/safefs"
)

const (
	file         = "announcement.json"
	maxFileBytes = 16 << 10

	KindNotice  = "notice"
	KindRelease = "release"

	// MaxMessageRunes bounds the banner text; it is one line or two, not a page.
	MaxMessageRunes = 500
	// MaxDurationMin bounds the expected interruption a release may announce.
	MaxDurationMin = 240
	// MaxLead is how far ahead a release may be scheduled.
	MaxLead = 30 * 24 * time.Hour
	// CompletedVisible is how long "upgraded to vX" stays up after the new
	// server starts.
	CompletedVisible = 10 * time.Minute
	// StaleAfter hides a release that never completed this long after its
	// scheduled start, so a forgotten banner does not say "upgrading" forever.
	StaleAfter = 24 * time.Hour
)

// Announcement is the stored record and the API payload. Times are Unix ms.
type Announcement struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
	// StartsAt is when a release is scheduled; 0 means "shortly".
	StartsAt    int64  `json:"starts_at,omitempty"`
	DurationMin int    `json:"duration_min,omitempty"`
	PostedAt    int64  `json:"posted_at"`
	PostedBy    string `json:"posted_by"`
	// FromVersion is the server version running when the release was
	// announced; a server starting with another version completes it.
	FromVersion      string `json:"from_version,omitempty"`
	CompletedAt      int64  `json:"completed_at,omitempty"`
	CompletedVersion string `json:"completed_version,omitempty"`
}

// Input is what an administrator or the CLI supplies.
type Input struct {
	Kind        string `json:"kind"`
	Message     string `json:"message"`
	StartsAt    int64  `json:"starts_at"`
	DurationMin int    `json:"duration_min"`
}

// ValidationError carries a message safe to show the administrator.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(msg string) error { return &ValidationError{Msg: msg} }

// Normalize trims and checks in against now.
func (in Input) Normalize(now time.Time) (Input, error) {
	in.Kind = strings.TrimSpace(in.Kind)
	in.Message = strings.TrimSpace(strings.ReplaceAll(in.Message, "\r\n", "\n"))
	switch in.Kind {
	case KindNotice:
		if in.Message == "" {
			return in, invalid("通告内容不能为空")
		}
		in.StartsAt, in.DurationMin = 0, 0
	case KindRelease:
		if in.StartsAt != 0 {
			at := time.UnixMilli(in.StartsAt)
			if at.Before(now.Add(-time.Hour)) || at.After(now.Add(MaxLead)) {
				return in, invalid("计划时间须在一小时前到 30 天内")
			}
		}
		if in.DurationMin < 0 || in.DurationMin > MaxDurationMin {
			return in, invalid("预计中断时间须在 0–240 分钟之间")
		}
	default:
		return in, invalid("通告类型无效")
	}
	if !utf8.ValidString(in.Message) || utf8.RuneCountInString(in.Message) > MaxMessageRunes {
		return in, invalid("通告内容最多 500 个字符")
	}
	return in, nil
}

// Visible reports whether a should still be shown at now.
func (a *Announcement) Visible(now time.Time) bool {
	if a == nil {
		return false
	}
	if a.CompletedAt != 0 {
		return now.Before(time.UnixMilli(a.CompletedAt).Add(CompletedVisible))
	}
	if a.Kind == KindRelease {
		start := a.StartsAt
		if start == 0 {
			start = a.PostedAt
		}
		return now.Before(time.UnixMilli(start).Add(StaleAfter))
	}
	return true
}

// Service reads and writes <data>/announcement.json.
type Service struct {
	data string
	mu   sync.Mutex
}

func New(data string) *Service { return &Service{data: data} }

// Get returns the stored announcement, or nil when there is none. Visibility
// is the caller's decision so the CLI can still show an expired record.
func (s *Service) Get() (*Announcement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, _, err := s.read()
	return a, err
}

// Revision is a digest of the file, "" when absent; it changes whenever any
// writer replaces or removes the announcement.
func (s *Service) Revision() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, raw, err := s.read()
	if err != nil || raw == nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8]), nil
}

func (s *Service) read() (*Announcement, []byte, error) {
	root, err := safefs.Open(s.data)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	raw, err := root.ReadAll(file, maxFileBytes)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, errors.New("无法读取系统通告")
	}
	var a Announcement
	if json.Unmarshal(raw, &a) != nil || a.Version != 1 || (a.Kind != KindNotice && a.Kind != KindRelease) {
		return nil, raw, errors.New("系统通告文件版本或内容无效")
	}
	return &a, raw, nil
}

func (s *Service) write(a *Announcement) error {
	raw, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	root, err := safefs.Open(s.data)
	if err != nil {
		return err
	}
	defer root.Close()
	_, err = root.WriteFile(file, raw, safefs.WriteOptions{Mode: 0600})
	return err
}

// Put replaces the announcement with a new one. A new ID makes browsers that
// dismissed the previous banner show this one.
func (s *Service) Put(in Input, by, fromVersion string, now time.Time) (*Announcement, error) {
	in, err := in.Normalize(now)
	if err != nil {
		return nil, err
	}
	var id [8]byte
	if _, err = rand.Read(id[:]); err != nil {
		return nil, err
	}
	a := &Announcement{
		Version: 1, ID: hex.EncodeToString(id[:]), Kind: in.Kind, Message: in.Message,
		StartsAt: in.StartsAt, DurationMin: in.DurationMin,
		PostedAt: now.UnixMilli(), PostedBy: by,
	}
	if in.Kind == KindRelease {
		a.FromVersion = fromVersion
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.write(a); err != nil {
		return nil, err
	}
	return a, nil
}

// Clear removes the announcement; removing a missing one is not an error.
func (s *Service) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	root, err := safefs.Open(s.data)
	if err != nil {
		return err
	}
	defer root.Close()
	if err = root.RemoveAll(file); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Complete is called once when a server starts. A pending release posted
// before this start completes when the version changed, or — when the poster
// did not know the old version — once its scheduled time has passed. It
// reports whether the record changed.
func (s *Service) Complete(version string, started time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, _, err := s.read()
	if err != nil || a == nil || a.Kind != KindRelease || a.CompletedAt != 0 {
		return false, err
	}
	if !time.UnixMilli(a.PostedAt).Before(started) {
		return false, nil
	}
	switch {
	case a.FromVersion != "" && a.FromVersion == version:
		return false, nil // restarted without upgrading
	case a.FromVersion == "" && started.UnixMilli() < a.StartsAt:
		return false, nil
	}
	a.CompletedAt, a.CompletedVersion = started.UnixMilli(), version
	return true, s.write(a)
}
