package backup

import (
	"context"
	"log/slog"
	"sync"

	"github.com/robfig/cron/v3"
)

// Scheduler runs backups on the cron schedules from the settings. Call Reload after saving settings.
type Scheduler struct {
	Svc *Service
	// OnDone is called after every scheduled run (for audit logging and failure mail).
	OnDone func(kind string, run *Run, err error)
	mu     sync.Mutex
	c      *cron.Cron
	active bool
}

// Start activates the scheduler on this server. In a cluster only the elected leader
// calls it, so each backup runs once.
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	s.active = true
	s.mu.Unlock()
	return s.Reload(ctx)
}

// Reload rereads the schedule. It does nothing while the scheduler is not active.
func (s *Scheduler) Reload(ctx context.Context) error {
	st, err := s.Svc.Load(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c != nil {
		s.c.Stop()
		s.c = nil
	}
	if !s.active {
		return nil
	}
	s.c = cron.New()
	add := func(spec, kind string) {
		if spec == "" {
			return
		}
		if _, err := s.c.AddFunc(spec, func() {
			run, err := s.Svc.Run(context.Background(), kind)
			if err == ErrBusy {
				return
			}
			if err != nil {
				slog.Error("scheduled backup failed", "kind", kind, "err", err)
			}
			if s.OnDone != nil {
				s.OnDone(kind, run, err)
			}
		}); err != nil {
			slog.Error("invalid backup schedule", "kind", kind, "spec", spec, "err", err)
		}
	}
	add(st.FullSchedule, "full")
	add(st.IncrementalSchedule, "incremental")
	s.c.Start()
	return nil
}

// Stop deactivates the scheduler, for example when this server loses leadership.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = false
	if s.c != nil {
		s.c.Stop()
		s.c = nil
	}
}

// ValidSchedule reports whether spec is empty or a valid 5-field cron expression.
func ValidSchedule(spec string) bool {
	if spec == "" {
		return true
	}
	_, err := cron.ParseStandard(spec)
	return err == nil
}
