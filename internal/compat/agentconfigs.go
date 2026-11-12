// SPDX-License-Identifier: Apache-2.0

package compat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/templates"
)

// agentConfigsDir holds <name>.json (the stored config as the writer made it) and
// <name>.decoded.json (what agentconfig.Parse made of it, marshalled again, at recording time).
const agentConfigsDir = "agent_configs"

// FullConfig is a hand-built config that sets every field of agentconfig.Config to a non-zero
// value, bindings and every optional field included: the shipped templates leave bindings and
// several omitempty fields empty, so only this one pins their stored names and types. A field added
// to Config must be added here; the harness self-test fails until it is. noTemplate drops the
// template reference, the shape of a version that was not made from a template.
func FullConfig(noTemplate bool) agentconfig.Config {
	c := agentconfig.Config{
		Template:     &agentconfig.TemplateRef{Slug: "compat", Version: "1.0.0", ContentHash: "sha256:koinon_test_not-a-real-hash"},
		Instructions: "Compat instructions.",
		Model:        "default",
		Thinking:     "on",
		Grants: []templates.Grant{{
			Connector: "github", Scopes: []string{"issues:read", "pulls:write"}, Preset: templates.PresetStandard,
			Targets: &templates.Targets{Min: 1, Max: 3}, Purpose: "compat",
		}},
		Bindings: []agentconfig.Binding{{
			Connector: "github", ConnectionID: "0198a000-0000-7000-8000-0000000000f1", Targets: []string{"acme/widgets", "acme/gadgets"},
		}},
		Limits: templates.Limits{
			Run:      templates.RunLimits{MaxDuration: templates.Duration{Duration: 30 * time.Minute}, MaxModelRequests: 200, MaxSpendUSD: 250},
			SpendUSD: 1500,
		},
		Triggers: []templates.Trigger{{
			ID: "nightly", Type: templates.TriggerSchedule, Filter: templates.TriggerFilter{Cron: "0 3 * * *", Timezone: "Europe/Tallinn"},
			Instruction: "Do the nightly thing.", Notify: &templates.NotifyTarget{Kind: templates.NotifyRole, Role: store.OrgRoleAdmin},
		}},
		Harness:   templates.HarnessSettings{PluginInstalls: true},
		Ephemeral: true,
		Questions: templates.QuestionTimers{ReminderAfter: templates.Duration{Duration: time.Hour}, ExpireAfter: templates.Duration{Duration: 48 * time.Hour}},
		Resources: templates.Resources{
			CPU:    templates.Quantities{Request: "250m", Limit: "1"},
			Memory: templates.Quantities{Request: "512Mi", Limit: "1Gi"},
		},
	}
	if noTemplate {
		c.Template = nil
	}
	return c
}

// recordAgentConfigs stores the config every shipped template makes with the current writer
// (agentconfig.FromTemplate and Marshal, the path CreateAgentFromTemplate takes), and the full
// configs of FullConfig.
func recordAgentConfigs(out string) error {
	dir := filepath.Join(out, agentConfigsDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	all, err := templates.All()
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return fmt.Errorf("no shipped template to record")
	}
	configs := map[string]agentconfig.Config{
		"full":             FullConfig(false),
		"full-no-template": FullConfig(true),
	}
	for _, t := range all {
		configs[t.Metadata.ID] = agentconfig.FromTemplate(t, t.Hash)
	}
	for name, c := range configs {
		raw, err := c.Marshal()
		if err != nil {
			return err
		}
		cfg, err := agentconfig.Parse(raw)
		if err != nil {
			return fmt.Errorf("config %s: %w", name, err)
		}
		decoded, err := cfg.Marshal()
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name+".json"), raw, 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name+".decoded.json"), decoded, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// CheckAgentConfigs decodes every recorded config with agentconfig.Parse, the path a stored
// agent version is read through, and requires the result to contain everything the recording release
// decoded (as a subset, so a field head adds is fine): a field the reader now drops, renames or
// changes shows as a difference.
func CheckAgentConfigs(dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, agentConfigsDir, "*.json"))
	if err != nil {
		return err
	}
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, ".decoded.json") {
			continue
		}
		n++
		raw, err := os.ReadFile(f) //nolint:gosec // G304: path inside the recorded release folder
		if err != nil {
			return err
		}
		want, err := os.ReadFile(strings.TrimSuffix(f, ".json") + ".decoded.json")
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		cfg, err := agentconfig.Parse(raw)
		if err != nil {
			return fmt.Errorf("%s no longer loads: %w", filepath.Base(f), err)
		}
		got, err := cfg.Marshal()
		if err != nil {
			return err
		}
		same, err := JSONContains(got, want)
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		if !same {
			return fmt.Errorf("%s decodes differently at head:\n got  %s\n want %s", filepath.Base(f), got, want)
		}
	}
	if n == 0 {
		return fmt.Errorf("%s holds no recorded config", agentConfigsDir)
	}
	return nil
}
