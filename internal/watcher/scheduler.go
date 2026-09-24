package watcher

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/cron"
	"github.com/Genentech/pinard/internal/pnats"
	"github.com/Genentech/pinard/internal/state"
)

type Scheduler struct {
	Runs      *state.Store[state.SchedulerRuns]
	NATS      *pnats.Client
	KV        *pnats.KV
	Vignoble  *config.Vignoble
	AOCBin    string
	Schedules []config.Schedule
}

func (s *Scheduler) Run() error {
	if len(s.Schedules) == 0 {
		return nil
	}

	now := time.Now()

	for i := range s.Schedules {
		sched := &s.Schedules[i]
		if !sched.Enabled {
			continue
		}

		var lastRun time.Time
		s.Runs.Read(func(r *state.SchedulerRuns) {
			if r.Runs != nil {
				if ts, ok := r.Runs[sched.Name]; ok {
					lastRun, _ = time.Parse(time.RFC3339, ts)
				}
			}
		})

		if !cron.ShouldFire(sched.Cron, sched.Name, lastRun, now) {
			continue
		}

		log.Printf("[scheduler] Schedule %s fires (project: %s)", sched.Name, sched.Project)

		var err error
		if sched.Command != "" {
			err = s.runCommand(sched)
		} else {
			err = s.spawn(sched)
		}

		if err != nil {
			errMsg := err.Error()
			if strings.Contains(errMsg, "already exists") || strings.Contains(errMsg, "Session already exists") {
				s.publishEvent(sched.Name, "skipped", map[string]any{
					"project": sched.Project,
					"reason":  "already running",
				})
			} else {
				s.publishEvent(sched.Name, "failed", map[string]any{
					"project": sched.Project,
					"error":   errMsg,
				})
			}
		} else {
			status := "spawned"
			if sched.Command != "" {
				status = "completed"
			}
			s.publishEvent(sched.Name, status, map[string]any{
				"project": sched.Project,
			})

			if sched.Once {
				sched.Enabled = false
			}
		}

		s.Runs.Update(func(r *state.SchedulerRuns) {
			if r.Runs == nil {
				r.Runs = make(map[string]string)
			}
			r.Runs[sched.Name] = now.Format(time.RFC3339)
		})
	}

	// Sync to KV
	for _, sched := range s.Schedules {
		key := fmt.Sprintf("%s-%s", s.Vignoble.Name, sched.Name)
		var lastRun string
		s.Runs.Read(func(r *state.SchedulerRuns) {
			if r.Runs != nil {
				lastRun = r.Runs[sched.Name]
			}
		})
		s.KV.Put("pinard-schedules", key, map[string]any{
			"name":     sched.Name,
			"project":  sched.Project,
			"cron":     sched.Cron,
			"enabled":  sched.Enabled,
			"last_run": lastRun,
			"vignoble": s.Vignoble.Name,
		})
	}

	return nil
}

func (s *Scheduler) runCommand(sched *config.Schedule) error {
	log.Printf("[scheduler] Running command: %s", sched.Command)
	cmd := exec.Command("sh", "-c", sched.Command)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, strings.TrimSpace(string(out)))
	}
	if len(out) > 0 {
		log.Printf("[scheduler] %s output: %s", sched.Name, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *Scheduler) spawn(sched *config.Schedule) error {
	// Name the worker per (schedule, firing period). The schedule name is unique
	// within the vignoble, and the period suffix matches the cron cadence, so the
	// name is stable within one period (a double-fire is correctly skipped) but
	// unique across periods. aoc uses --name verbatim (no random suffix).
	name := fmt.Sprintf("%s-%s-%s", s.Vignoble.Name, sanitizeName(sched.Name), cron.PeriodSuffix(sched.Cron, time.Now()))
	args := []string{"spawn", "--project", sched.Project, "--name", name, "--ephemeral"}
	// Base the worktree on the vigne's default branch (mirrors the issue
	// watcher). Without this, aoc spawn falls back to "main"/"origin/main",
	// which breaks scheduled spawns on repos whose default branch isn't main.
	if v, ok := s.Vignoble.Config.Vignes[sched.Project]; ok {
		args = append(args, "--target-branch", v.TargetBranch())
	}
	if sched.Prompt != "" {
		args = append(args, "--prompt", sched.Prompt)
	} else {
		args = append(args, "--prompt", fmt.Sprintf("Scheduled task: %s", sched.Name))
	}

	cmd := exec.Command(s.AOCBin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sanitizeName makes a schedule name safe to use as a tmux session and git
// branch component (alphanumerics, '-' and '_' only).
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

func (s *Scheduler) publishEvent(name, status string, data map[string]any) {
	subject := fmt.Sprintf("pinard.%s.schedules.%s.%s", s.Vignoble.Name, name, status)
	s.NATS.Publish(subject, data)
}
