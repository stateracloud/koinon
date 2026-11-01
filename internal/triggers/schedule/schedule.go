// SPDX-License-Identifier: Apache-2.0

// Package schedule fires the schedule triggers of active agents (plan item TR2, ADR 0066).
//
// Every replica of koinon-api runs a Scheduler (with api.durable.enabled). It holds no schedule
// state of its own: on each tick it reads the triggers in effect (internal/triggers), works out
// the due times that fall in the window since the last tick, and enqueues one DBOS workflow per
// due time on the org-partitioned queue (durable.EnqueueForOrg). The workflow ID and the dedupe
// key (durable.TriggerFireKey) both name the trigger and the due time, so a due time fires once
// across replicas, ticks that overlap and restarts. A changed or removed trigger simply stops
// producing due times, because the next tick reads the active version.
//
// Timing, per (agent, due time): the run starts Jitter(agent, due) after the due time (0 up to
// JitterWindow, deterministic, so agents due in the same minute spread out), and the agent's
// sandbox is woken Lead before that start (wake.CronDue).
//
// A due time missed while no replica ran fires at most once, on start: the latest start time
// within CatchUp, and never before the version that defines the trigger was activated.
package schedule

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/robfig/cron/v3"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
	"github.com/stateracloud/koinon/internal/services"
	"github.com/stateracloud/koinon/internal/triggers"
	"github.com/stateracloud/koinon/internal/wake"
	"github.com/stateracloud/koinon/templates"
)

// Defaults (recorded in ADR 0066's TR2 amendment; all are Config fields).
const (
	// DefaultLead is how long before a scheduled run starts its sandbox is woken.
	DefaultLead = 2 * time.Minute
	// JitterWindow is the spread of start times after a due time.
	JitterWindow = 30 * time.Second
	// DefaultInterval is how often a replica looks for due times.
	DefaultInterval = 5 * time.Second
	// DefaultCatchUp is how far back a start looks for the one missed due time.
	DefaultCatchUp = time.Hour
	// FireWorkflow is the registered name of the fire workflow.
	FireWorkflow = "koinon.trigger-fire"

	maxDuesPerTrigger = 5000
)

// Config configures a Scheduler. Zero values take the defaults.
type Config struct {
	Lead     time.Duration
	Interval time.Duration
	CatchUp  time.Duration
	// Now is the clock; nil is time.Now. Tests pass a controllable one.
	Now    func() time.Time
	Logger *slog.Logger
	// Wake pre-wakes the agent's sandbox; nil is wake.CronDue. Tests observe it.
	Wake func(ctx context.Context, q *store.Queries, org, agent pgtype.UUID) (bool, error)
}

// Creator creates the task of a fire: *services.Server.
type Creator interface {
	CreateTriggeredTask(ctx context.Context, in services.TriggeredTask) (store.Task, bool, error)
}

// FireInput is the input of the fire workflow: the identity of one due time, not its content, so
// the instruction is the one of the version in effect when the workflow runs.
type FireInput struct {
	Org     string `json:"org"`
	Agent   string `json:"agent"`
	Trigger string `json:"trigger"`
	Version int32  `json:"version"`
	// Cron, Timezone and Instruction are the trigger's content when the due time was enqueued; the
	// fire goes ahead only while the trigger in effect still has the same content.
	Cron        string `json:"cron"`
	Timezone    string `json:"timezone"`
	Instruction string `json:"instruction"`
	// Due is the scheduled due time in Unix seconds (not the jittered start).
	Due int64 `json:"due"`
}

// Scheduler fires schedule triggers.
type Scheduler struct {
	cfg  Config
	pool *pgxpool.Pool
	q    *store.Queries
	work *durable.OrgWork
	log  *slog.Logger

	bound chan struct{}
	once  sync.Once
	tasks Creator

	mu     sync.Mutex
	cursor time.Time // end of the last processed window; zero before the first tick
}

// New returns a Scheduler enqueuing on work. Add Registrar to the durable runtime before Launch,
// then call Bind once the services exist and Run after Launch.
func New(cfg Config, pool *pgxpool.Pool, work *durable.OrgWork) *Scheduler {
	if cfg.Lead <= 0 {
		cfg.Lead = DefaultLead
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.CatchUp <= 0 {
		cfg.CatchUp = DefaultCatchUp
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Wake == nil {
		cfg.Wake = wake.CronDue
	}
	return &Scheduler{cfg: cfg, pool: pool, q: store.New(pool), work: work, log: cfg.Logger, bound: make(chan struct{})}
}

// Bind sets the task creator. A fire workflow recovered at launch waits for it.
func (s *Scheduler) Bind(c Creator) {
	s.once.Do(func() {
		s.tasks = c
		close(s.bound)
	})
}

// Registrar registers the fire workflow for durable.Runtime.Register.
func (s *Scheduler) Registrar() durable.Registrar {
	return func(c dbos.Context) error {
		dbos.RegisterWorkflow(c, s.fire, dbos.WithWorkflowName(FireWorkflow))
		return nil
	}
}

// Run ticks until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		if err := s.Tick(ctx, s.cfg.Now()); err != nil && ctx.Err() == nil {
			s.log.Warn("schedule triggers: tick failed, retrying the same window", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Jitter is the delay between a due time and the start of its run: deterministic per agent and
// due time, spread over [0, JitterWindow).
func Jitter(agent pgtype.UUID, due time.Time) time.Duration {
	h := sha256.New()
	h.Write(agent.Bytes[:])
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(due.UTC().Unix())) //nolint:gosec // Unix seconds of a real date are positive
	h.Write(b[:])
	sum := h.Sum(nil)
	return time.Duration(binary.BigEndian.Uint64(sum[:8])%uint64(JitterWindow/time.Millisecond)) * time.Millisecond
}

// Tick processes the window from the previous tick (or, the first time, CatchUp back) to now.
// The window only advances when the tick succeeded.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	from := s.cursor
	if from.IsZero() {
		from = now.Add(-s.cfg.CatchUp)
	}
	if !now.After(from) {
		return nil
	}
	list, skipped, err := triggers.AllLenient(ctx, s.q, templates.TriggerSchedule)
	if err != nil {
		return err
	}
	for _, sk := range skipped {
		s.log.Warn("schedule triggers: agent config does not parse, its schedules are skipped",
			"org", sk.OrgID.String(), "agent", sk.AgentID.String(), "version", sk.Version, "err", sk.Err)
	}
	acts, err := s.q.ListAgentActivationsAllOrgs(ctx)
	if err != nil {
		return fmt.Errorf("list activations: %w", err)
	}
	activated := map[pgtype.UUID]time.Time{}
	for _, a := range acts {
		activated[a.AgentID] = a.ActivatedAt.Time
	}
	woken := map[pgtype.UUID]bool{}
	for _, tr := range list {
		sched, loc, err := templates.ParseSchedule(tr.Filter.Cron, tr.Filter.Timezone)
		if err != nil {
			s.log.Warn("schedule triggers: schedule does not parse, skipped",
				"org", tr.OrgID.String(), "agent", tr.AgentID.String(), "trigger", tr.ID, "err", err)
			continue
		}
		fire, wakeAgent := s.plan(tr, sched, loc, activated[tr.AgentID], from, now)
		if wakeAgent && !woken[tr.AgentID] {
			woken[tr.AgentID] = true
			if _, err := s.cfg.Wake(ctx, s.q, tr.OrgID, tr.AgentID); err != nil {
				s.log.Warn("schedule triggers: pre-wake failed", "agent", tr.AgentID.String(), "err", err)
			}
		}
		if fire.IsZero() {
			continue
		}
		in := FireInput{
			Org: tr.OrgID.String(), Agent: tr.AgentID.String(), Trigger: tr.ID, Version: tr.Version,
			Cron: tr.Filter.Cron, Timezone: tr.Filter.Timezone, Instruction: tr.Instruction, Due: fire.Unix(),
		}
		key := durable.TriggerFireKey(triggers.UUID(tr.AgentID, tr.ID).String(), fire)
		if _, err := durable.EnqueueForOrg[string](ctx, s.work, in.Org, FireWorkflow, in, dbos.WithEnqueueWorkflowID("trigger-fire:"+in.Org+":"+key)); err != nil {
			return fmt.Errorf("enqueue fire of %s/%s: %w", in.Agent, tr.ID, err)
		}
	}
	s.cursor = now
	return nil
}

// plan returns, for one trigger and the window (from, to], the due time to fire (the latest one
// whose start falls in the window; zero for none) and whether the agent's sandbox is to be woken
// (a run is coming but has not started).
func (s *Scheduler) plan(tr triggers.Active, sched cron.Schedule, loc *time.Location, activated, from, to time.Time) (fire time.Time, wakeAgent bool) {
	next := func(t time.Time) (time.Time, bool) {
		n := sched.Next(t.In(loc))
		return n, !n.IsZero()
	}
	// Starts follow due times by less than JitterWindow, so due times after from-JitterWindow
	// are the only candidates; wakes are Lead earlier than starts.
	d, ok := next(from.Add(-JitterWindow))
	for n := 0; ok && !d.After(to.Add(s.cfg.Lead)) && n < maxDuesPerTrigger; n++ {
		if d.After(activated) {
			start := d.Add(Jitter(tr.AgentID, d))
			switch {
			case start.After(from) && !start.After(to):
				fire = d
			case start.After(to) && start.Add(-s.cfg.Lead).After(from) && !start.Add(-s.cfg.Lead).After(to):
				wakeAgent = true
			}
		}
		d, ok = next(d)
	}
	return fire, wakeAgent
}

const stepCreateTask = "create-task"

// fire is the workflow of one due time.
func (s *Scheduler) fire(c dbos.Context, in FireInput) (string, error) {
	select {
	case <-s.bound:
	case <-c.Done():
		return "", c.Err()
	}
	id, err := dbos.RunAsStep(c, func(ctx context.Context) (string, error) { return s.createTask(ctx, in) },
		dbos.WithStepName(stepCreateTask), dbos.WithStepMaxRetries(5))
	return id, err
}

func (s *Scheduler) createTask(ctx context.Context, in FireInput) (string, error) {
	var org, agent pgtype.UUID
	if err := org.Scan(in.Org); err != nil {
		return "", fmt.Errorf("org: %w", err)
	}
	if err := agent.Scan(in.Agent); err != nil {
		return "", fmt.Errorf("agent: %w", err)
	}
	// Only a trigger still in effect fires: a version activated since the due time was enqueued
	// may have changed or removed it. The trigger's content is compared, not the version number:
	// an unrelated version (an instructions edit, another trigger) activated between the due time
	// and the start must not drop the run. The version is for the log only.
	cur, err := triggers.ForAgent(ctx, s.q, org, agent, templates.TriggerSchedule)
	if err != nil {
		return "", err
	}
	var tr *triggers.Active
	for i := range cur {
		if cur[i].ID == in.Trigger && sameSchedule(cur[i].Trigger, in) {
			tr = &cur[i]
		}
	}
	if tr == nil {
		s.log.Info("schedule triggers: trigger changed or removed before it fired, dropped", "agent", in.Agent, "trigger", in.Trigger, "version", in.Version)
		return "", nil
	}
	due := time.Unix(in.Due, 0).UTC()
	trig := triggers.UUID(agent, in.Trigger)
	_, loc, perr := templates.ParseSchedule(tr.Filter.Cron, tr.Filter.Timezone)
	if perr != nil {
		loc = time.UTC
	}
	task, _, err := s.tasks.CreateTriggeredTask(ctx, services.TriggeredTask{
		Org: org, Agent: agent, Trigger: trig,
		Source: durable.SourceTrigger, Key: durable.TriggerFireKey(trig.String(), due),
		Title:   triggers.ScheduleTaskTitle(in.Trigger, due, loc),
		Message: tr.Instruction,
		// The target is the one of the version in effect at the fire, like the instruction.
		Notify: tr.Notify,
		Origin: store.RunOriginScheduled,
		Fire:   triggers.FireScheduled,
	})
	if errors.Is(err, services.ErrAgentInactive) {
		return "", nil // permanent: the agent was paused or archived since
	}
	if err != nil {
		return "", err
	}
	return task.ID.String(), nil
}

func sameSchedule(t templates.Trigger, in FireInput) bool {
	return t.Filter.Cron == in.Cron && t.Filter.Timezone == in.Timezone && t.Instruction == in.Instruction
}
