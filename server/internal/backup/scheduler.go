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
}

func (s *Scheduler) Reload(ctx context.Context) error {
	st, err := s.Svc.Load(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c != nil {
		s.c.Stop()
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

func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c != nil {
		s.c.Stop()
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
