// SPDX-License-Identifier: Apache-2.0

package compat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/templates"
)

// agentConfigsDir holds <name>.json (the stored config as the writer made it) and
// <name>.decoded.json (what agentconfig.Parse made of it, marshalled again, at recording time).
const agentConfigsDir = "agent_configs"

// recordAgentConfigs stores the config every shipped template makes with the current writer
// (agentconfig.FromTemplate and Marshal, the path CreateAgentFromTemplate takes).
func recordAgentConfigs(out string) error {
	dir := filepath.Join(out, agentConfigsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	all, err := templates.All()
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return fmt.Errorf("no shipped template to record")
	}
	for _, t := range all {
		raw, err := agentconfig.FromTemplate(t, t.Hash).Marshal()
		if err != nil {
			return err
		}
		cfg, err := agentconfig.Parse(raw)
		if err != nil {
			return fmt.Errorf("template %s: %w", t.Metadata.ID, err)
		}
		decoded, err := cfg.Marshal()
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, t.Metadata.ID+".json"), raw, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, t.Metadata.ID+".decoded.json"), decoded, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// CheckAgentConfigs decodes every recorded config with agentconfig.Parse, the path a stored
// agent version is read through, and requires the result to equal what the recording release
// decoded: a field the reader now drops or renames shows as a difference.
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
		raw, err := os.ReadFile(f)
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
		same, err := sameJSON(got, want)
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
