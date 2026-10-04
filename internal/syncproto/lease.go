package syncproto

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"strings"
	"sync"
	"time"
)

const LeaseTTL = 30 * time.Second
const MaxLeases = 256

var ErrLeaseBusy = errors.New("another sync writer holds an overlapping lease")
var ErrLeaseExpired = errors.New("sync lease is missing, expired or superseded")

type Lease struct {
	Workspace  string    `json:"workspace"`
	Project    string    `json:"project"`
	Path       string    `json:"path"`
	Device     string    `json:"device"`
	Token      string    `json:"token"`
	Generation string    `json:"generation"`
	Expires    time.Time `json:"expires_at"`
}

// Leases are intentionally process-scoped. Restart destroys every grant; a
// previous process's token/generation can never authorize a new write. HTTP
// callers serialize acquire/check/mutation with the workspace lifecycle lock.
type Leases struct {
	mu      sync.Mutex
	entries map[string]Lease
	now     func() time.Time
}

func NewLeases() *Leases { return &Leases{entries: map[string]Lease{}, now: time.Now} }

func leaseKey(workspace, project string) string { return workspace + "\x00" + project }
func validIdentity(value string) bool {
	return value != "" && len(value) <= 128 && !strings.ContainsAny(value, "\x00\r\n")
}
func overlap(a, b string) bool {
	return a == "." || b == "." || a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
func (l *Leases) sweep() {
	for key, value := range l.entries {
		if !l.now().Before(value.Expires) {
			delete(l.entries, key)
		}
	}
}

func (l *Leases) Acquire(workspace, project, directory, device string) (Lease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep()
	if !validIdentity(workspace) || !validIdentity(project) || !validIdentity(device) || (directory != "." && !ValidPath(directory)) {
		return Lease{}, ErrInvalid
	}
	for _, lease := range l.entries {
		if lease.Workspace == workspace && overlap(lease.Path, directory) {
			return Lease{}, ErrLeaseBusy
		}
	}
	if len(l.entries) >= MaxLeases {
		return Lease{}, ErrLimit
	}
	lease := Lease{Workspace: workspace, Project: project, Path: directory, Device: device, Token: rand.Text(), Generation: rand.Text(), Expires: l.now().Add(LeaseTTL)}
	l.entries[leaseKey(workspace, project)] = lease
	return lease, nil
}

func (l *Leases) check(workspace, project, directory, device, token, generation string) (Lease, error) {
	l.sweep()
	lease, ok := l.entries[leaseKey(workspace, project)]
	if !ok || lease.Device != device || lease.Path != directory || subtle.ConstantTimeCompare([]byte(token), []byte(lease.Token)) != 1 || subtle.ConstantTimeCompare([]byte(generation), []byte(lease.Generation)) != 1 {
		return Lease{}, ErrLeaseExpired
	}
	return lease, nil
}
func (l *Leases) Check(workspace, project, directory, device, token, generation string) (Lease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.check(workspace, project, directory, device, token, generation)
}
func (l *Leases) Renew(workspace, project, directory, device, token, generation string) (Lease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	lease, err := l.check(workspace, project, directory, device, token, generation)
	if err != nil {
		return Lease{}, err
	}
	lease.Expires = l.now().Add(LeaseTTL)
	l.entries[leaseKey(workspace, project)] = lease
	return lease, nil
}
func (l *Leases) Release(workspace, project, directory, device, token, generation string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.check(workspace, project, directory, device, token, generation); err != nil {
		return err
	}
	delete(l.entries, leaseKey(workspace, project))
	return nil
}
func (l *Leases) Active(workspace, project string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep()
	_, ok := l.entries[leaseKey(workspace, project)]
	return ok
}

// WorkspaceActive is used by journal maintenance, including receipts for
// projects that have since been removed. No active executor may lose old bytes.
func (l *Leases) WorkspaceActive(workspace string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep()
	for _, lease := range l.entries {
		if lease.Workspace == workspace {
			return true
		}
	}
	return false
}
