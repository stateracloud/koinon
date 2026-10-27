// SPDX-License-Identifier: Apache-2.0

package templates

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // IANA zones are validated and used even where the image has no zoneinfo

	"github.com/robfig/cron/v3"
)

// TriggerType says what starts work on an agent (ADR 0066).
type TriggerType string

// Trigger types. Schedule and direct message are built in v0.1; the other two are reserved for
// v0.2 and rejected until then.
const (
	TriggerSchedule      TriggerType = "schedule"
	TriggerDirectMessage TriggerType = "direct_message"
	TriggerCodeHostEvent TriggerType = "code_host_event"
	TriggerChannelEvent  TriggerType = "channel_event"
)

// Trigger limits.
const (
	MaxTriggers           = 16
	MaxTriggerInstruction = 4096
)

// Trigger is one way work starts on an agent, with the instruction the agent gets when it fires.
// Triggers live in the versioned agent config and in templates, and take effect when a version
// that holds them is activated (ADR 0066 decision 4).
type Trigger struct {
	// ID names the trigger within its agent: a slug, stable across versions so a schedule keeps
	// its identity when the instruction changes.
	ID          string        `json:"id"`
	Type        TriggerType   `json:"type"`
	Filter      TriggerFilter `json:"filter,omitzero"`
	Instruction string        `json:"instruction"`
}

// TriggerFilter is the filter of a trigger, typed per trigger type. There is no expression
// language in v0.1. A schedule uses Cron and Timezone; a direct message has no filter.
type TriggerFilter struct {
	// Cron is a standard five-field expression (minute hour day-of-month month day-of-week).
	Cron string `json:"cron,omitempty"`
	// Timezone is an IANA zone name such as Europe/Tallinn.
	Timezone string `json:"timezone,omitempty"`
}

var triggerIDRE = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// ParseSchedule parses a trigger's cron expression and timezone.
func ParseSchedule(expr, zone string) (cron.Schedule, *time.Location, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, nil, fmt.Errorf("cron is required")
	}
	if strings.HasPrefix(expr, "@") || strings.HasPrefix(expr, "TZ=") || strings.HasPrefix(expr, "CRON_TZ=") {
		return nil, nil, fmt.Errorf("cron %q: use five fields; the timezone is a separate setting", expr)
	}
	s, err := cronParser.Parse(expr)
	if err != nil {
		return nil, nil, fmt.Errorf("cron %q: %w", expr, err)
	}
	if zone == "" || zone == "Local" {
		return nil, nil, fmt.Errorf("timezone must be an IANA zone name such as Europe/Tallinn")
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, nil, fmt.Errorf("timezone %q is not an IANA zone", zone)
	}
	return s, loc, nil
}

// Next returns the first time after t the schedule fires, in the trigger's timezone.
// It reports false for a trigger that is not a valid schedule.
func (tr Trigger) Next(t time.Time) (time.Time, bool) {
	if tr.Type != TriggerSchedule {
		return time.Time{}, false
	}
	s, loc, err := ParseSchedule(tr.Filter.Cron, tr.Filter.Timezone)
	if err != nil {
		return time.Time{}, false
	}
	n := s.Next(t.In(loc))
	return n, !n.IsZero()
}

// ValidateTriggers checks an agent version's or template's triggers.
func ValidateTriggers(ts []Trigger) error {
	if len(ts) > MaxTriggers {
		return fmt.Errorf("triggers: at most %d", MaxTriggers)
	}
	seen := map[string]bool{}
	directs := 0
	for i, tr := range ts {
		bad := func(format string, a ...any) error {
			return fmt.Errorf("triggers[%d]: %s", i, fmt.Sprintf(format, a...))
		}
		if len(tr.ID) > 63 || !triggerIDRE.MatchString(tr.ID) {
			return bad("id %q must be a lowercase slug of at most 63 characters", tr.ID)
		}
		if seen[tr.ID] {
			return bad("duplicate id %q", tr.ID)
		}
		seen[tr.ID] = true
		if strings.TrimSpace(tr.Instruction) == "" {
			return bad("instruction is required")
		}
		if len(tr.Instruction) > MaxTriggerInstruction {
			return bad("instruction is longer than %d characters", MaxTriggerInstruction)
		}
		switch tr.Type {
		case TriggerSchedule:
			if _, _, err := ParseSchedule(tr.Filter.Cron, tr.Filter.Timezone); err != nil {
				return bad("%v", err)
			}
		case TriggerDirectMessage:
			if tr.Filter != (TriggerFilter{}) {
				return bad("a direct_message trigger has no filter")
			}
			if directs++; directs > 1 {
				return bad("only one direct_message trigger")
			}
		case TriggerCodeHostEvent, TriggerChannelEvent:
			return bad("type %q is reserved and not available yet", tr.Type)
		default:
			return bad("unknown type %q", tr.Type)
		}
	}
	return nil
}
