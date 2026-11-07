// SPDX-License-Identifier: Apache-2.0

// Package pricefetch reads public model price lists and turns them into price offers (ADR 0098).
//
// A source adapter fetches one list, checks it, and normalizes every model's price to Koinon's
// units: USD per million tokens, decimal strings with at most six fraction digits (package
// pricing). The classification of a difference between an offer and the price Koinon uses today,
// and the bounds that decide whether it may apply by itself, are pure functions in rules.go.
// Nothing here touches the database or the clock beyond the fetch time it is given.
//
// Only OpenRouter has an adapter so far (slice 1 of #265). The adapters for models.dev, LiteLLM
// and genai-prices are later slices.
package pricefetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/stateracloud/koinon/pricing"
)

// Source ids: the fixed set of adapters (also the values of the metrics' source label).
const (
	SourceOpenRouter = "openrouter"
	// SourceManual marks a price a person typed from a provider page.
	SourceManual = "manual"
)

// Sources lists the adapters. Metric labels use only these.
var Sources = []string{SourceOpenRouter}

// Licences recorded in the provenance (ADR 0098 decision 4).
const LicenceOpenRouter = "OpenRouter terms"

// MaxBodyBytes caps the body of a source (ADR 0098 decision 2.3).
const MaxBodyBytes = 16 << 20

// Offer is what a source says one model costs, normalized.
type Offer struct {
	Model string `json:"model"`
	// ProviderType is the provider type a Koinon price for it applies to.
	ProviderType string `json:"providerType"`
	// Spec holds USD per million tokens as decimal strings; tiers use Koinon's context_over.
	Spec pricing.Spec `json:"spec"`
	// MaxOutput is the largest output in tokens the source reports; 0 when it reports none.
	MaxOutput int `json:"maxOutput,omitempty"`
	// Ignored is data the source gives that Koinon cannot bill (ADR 0098 decisions 2.7 and 8). It is
	// kept for the provenance and never becomes a rate; a difference only in it makes no proposal.
	Ignored map[string]string `json:"ignored,omitempty"`
	// Problems say why no entry can be made from the source's data for this model. Such an offer
	// becomes a refused proposal.
	Problems []string `json:"problems,omitempty"`
}

// Snapshot is one fetch of one source.
type Snapshot struct {
	Source    string    `json:"source"`
	URL       string    `json:"url"`
	Licence   string    `json:"licence"`
	FetchedAt time.Time `json:"fetchedAt"`
	// SHA256 is the hex sha256 of the raw body.
	SHA256 string `json:"sha256"`
	// ETag and CommitSHA are the source's own version marks, where it has one.
	ETag      string `json:"etag,omitempty"`
	CommitSHA string `json:"commitSha,omitempty"`
	// Count is how many models the body listed, whether or not they could be normalized.
	Count  int     `json:"count"`
	Offers []Offer `json:"offers"`
}

// Adapter fetches one source.
type Adapter interface {
	// Name is the source id.
	Name() string
	// Fetch downloads and normalizes the list. A body that cannot be used is an error; the model-count
	// floors are checked by CheckFloor, which needs the last good fetch.
	Fetch(ctx context.Context, now time.Time) (Snapshot, error)
}

// RefusedError is a body that parsed but fails a floor: too few models, or fewer than half of the
// last good fetch's. It is recorded as a refused fetch.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return "pricefetch: refused: " + e.Reason }

// CheckFloor refuses a snapshot with fewer than minModels models or fewer than half the models of
// the last good fetch (lastGood 0: there was none).
func CheckFloor(s Snapshot, minModels, lastGood int) error {
	if s.Count < minModels {
		return &RefusedError{Reason: fmt.Sprintf("%s listed %d models, below the floor of %d", s.Source, s.Count, minModels)}
	}
	if lastGood > 0 && s.Count*2 < lastGood {
		return &RefusedError{Reason: fmt.Sprintf("%s listed %d models, fewer than half of the last good fetch's %d", s.Source, s.Count, lastGood)}
	}
	return nil
}

// DigestBody is the hex sha256 of a raw body.
func DigestBody(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SpecHash is the hex sha256 of a spec's canonical JSON: the identity of a set of values, for the
// stability rule and the dismissed-proposal memory.
func SpecHash(s pricing.Spec) string {
	b, _ := json.Marshal(s) // a struct of strings and integers always marshals
	return DigestBody(b)
}

// File is the air-gapped exchange format written by "prices fetch --out" and read by "prices
// import": the normalized snapshots, each with its provenance header.
type File struct {
	Format    string     `json:"format"`
	Snapshots []Snapshot `json:"snapshots"`
}

// FileFormat identifies the exchange format and its version.
const FileFormat = "koinon-price-snapshots/1"

// EncodeFile writes snapshots in the exchange format.
func EncodeFile(snaps []Snapshot) ([]byte, error) {
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Source < snaps[j].Source })
	return json.MarshalIndent(File{Format: FileFormat, Snapshots: snaps}, "", "  ")
}

// DecodeFile reads the exchange format strictly: an unknown field, another format version or
// trailing data is an error. It checks each snapshot's source and that every offer is for a
// provider type and a model name; the price rules run again when proposals are made.
func DecodeFile(r io.Reader) (File, error) {
	body, err := io.ReadAll(io.LimitReader(r, MaxBodyBytes+1))
	if err != nil {
		return File{}, fmt.Errorf("pricefetch: read: %w", err)
	}
	if len(body) > MaxBodyBytes {
		return File{}, errors.New("pricefetch: the file is larger than 16 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	var f File
	if err := d.Decode(&f); err != nil {
		return File{}, fmt.Errorf("pricefetch: decode: %w", err)
	}
	if d.More() {
		return File{}, errors.New("pricefetch: trailing data after the file")
	}
	if f.Format != FileFormat {
		return File{}, fmt.Errorf("pricefetch: format %q is not %q", f.Format, FileFormat)
	}
	for i, s := range f.Snapshots {
		if !knownSource(s.Source) {
			return File{}, fmt.Errorf("pricefetch: snapshot %d: unknown source %q", i, s.Source)
		}
		for _, o := range s.Offers {
			if o.Model == "" || o.ProviderType == "" {
				return File{}, fmt.Errorf("pricefetch: snapshot %d: an offer without a model or provider type", i)
			}
		}
	}
	return f, nil
}

func knownSource(s string) bool {
	for _, k := range Sources {
		if k == s {
			return true
		}
	}
	return false
}
