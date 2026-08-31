package reconcile

import (
	"log/slog"
	"time"
)

type Runner struct {
	interval time.Duration
	callback func()
}

func NewRunner(interval time.Duration, callback func()) *Runner {
	if interval <= 0 {
		interval = 45 * time.Second
	}
	return &Runner{interval: interval, callback: callback}
}

func (r *Runner) Start() {
	if r == nil || r.callback == nil {
		return
	}
	go func() {
		for {
			time.Sleep(r.interval)
			slog.Info("reconcile tick")
			r.callback()
		}
	}()
}
