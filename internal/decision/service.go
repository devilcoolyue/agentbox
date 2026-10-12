package decision

import (
	"sync"
	"time"
)

// Service limits physical attempts. Acquire happens before durable reservation;
// it never holds its lock while calling storage or a provider. Config changes
// reset failure history but do not forget slots still held by old requests.
type Service struct {
	mu        sync.Mutex
	revision  int64
	active    int
	failures  int
	openUntil time.Time
	probe     bool
	now       func() time.Time
}

func NewService() *Service { return &Service{now: time.Now} }
func (s *Service) Acquire(revision int64, maxConcurrency int) (func(bool), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if revision < 1 || maxConcurrency < 1 || maxConcurrency > 16 {
		return nil, ErrInvalid
	}
	if s.now == nil {
		s.now = time.Now
	}
	if revision < s.revision {
		return nil, ErrGeneration
	}
	if revision > s.revision {
		s.revision = revision
		s.failures = 0
		s.openUntil = time.Time{}
		s.probe = false
	}
	now := s.now()
	if !s.openUntil.IsZero() && (now.Before(s.openUntil) || s.probe) {
		return nil, ErrCircuitOpen
	}
	if s.active >= maxConcurrency {
		return nil, ErrBusy
	}
	if !s.openUntil.IsZero() {
		s.probe = true
	}
	s.active++
	var once sync.Once
	return func(failed bool) {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.active--
			if revision != s.revision {
				return
			}
			s.probe = false
			if failed {
				s.failures++
				if s.failures >= 3 {
					s.openUntil = s.now().Add(30 * time.Second)
				}
			} else {
				s.failures = 0
				s.openUntil = time.Time{}
			}
		})
	}, nil
}
