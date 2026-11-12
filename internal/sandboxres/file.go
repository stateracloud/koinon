// SPDX-License-Identifier: Apache-2.0

package sandboxres

import "fmt"

// The chart's config.yaml section "sandboxResources", read by koinon-reconciler and koinon-api
// (the API refuses a per-agent override outside the bounds, the reconciler applies it).

// FileConfig is the chart's sandbox.resources (ADR 0083). Values left out keep
// DefaultPolicy's.
type FileConfig struct {
	Default struct {
		CPU    quantitiesConfig `mapstructure:"cpu"`
		Memory quantitiesConfig `mapstructure:"memory"`
	} `mapstructure:"default"`
	Bounds struct {
		CPU struct {
			Request rangeConfig `mapstructure:"request"`
			Limit   rangeConfig `mapstructure:"limit"`
		} `mapstructure:"cpu"`
		Memory struct {
			Request rangeConfig `mapstructure:"request"`
			Limit   rangeConfig `mapstructure:"limit"`
		} `mapstructure:"memory"`
	} `mapstructure:"bounds"`
	MemoryRequestEqualsLimit *bool `mapstructure:"memoryRequestEqualsLimit"`
}

type quantitiesConfig struct {
	Request string `mapstructure:"request"`
	Limit   string `mapstructure:"limit"`
}

type rangeConfig struct {
	Min string `mapstructure:"min"`
	Max string `mapstructure:"max"`
}

// Policy lays the file's sandbox resources over DefaultPolicy: a value the file leaves
// empty keeps the default.
func (sr FileConfig) Policy() Policy {
	p := DefaultPolicy()
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&p.Default.CPU.Request, sr.Default.CPU.Request)
	set(&p.Default.CPU.Limit, sr.Default.CPU.Limit)
	set(&p.Default.Memory.Request, sr.Default.Memory.Request)
	set(&p.Default.Memory.Limit, sr.Default.Memory.Limit)
	for _, r := range []struct {
		dst *Range
		src rangeConfig
	}{
		{&p.Bounds.CPURequest, sr.Bounds.CPU.Request},
		{&p.Bounds.CPULimit, sr.Bounds.CPU.Limit},
		{&p.Bounds.MemoryRequest, sr.Bounds.Memory.Request},
		{&p.Bounds.MemoryLimit, sr.Bounds.Memory.Limit},
	} {
		set(&r.dst.Min, r.src.Min)
		set(&r.dst.Max, r.src.Max)
	}
	if sr.MemoryRequestEqualsLimit != nil {
		p.MemoryRequestEqualsLimit = *sr.MemoryRequestEqualsLimit
	}
	return p
}

// FromConfig reads the "sandboxResources" section with unmarshalKey (viper's UnmarshalKey), lays
// it over the built-in policy and validates the result.
func FromConfig(unmarshalKey func(key string, out any) error) (Policy, error) {
	var sr FileConfig
	if err := unmarshalKey("sandboxResources", &sr); err != nil {
		return Policy{}, fmt.Errorf("config sandboxResources: %w", err)
	}
	p := sr.Policy()
	if err := p.Validate(); err != nil {
		return Policy{}, fmt.Errorf("config sandboxResources: %w", err)
	}
	return p, nil
}
