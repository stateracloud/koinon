// SPDX-License-Identifier: Apache-2.0

package prices

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/pricing"
)

// Migration 00014 turned each model's rates into an org price entry in SQL, which cannot read the
// default list. It left the note NotePending + <model name> and generic estimation settings.
const (
	// MigrationActor is the created_by of the entries migration 00014 made.
	MigrationActor = "migration 00014"
	// NotePending marks an entry that CompleteMigrated has not filled yet.
	NotePending = "migrated:pending:"
	// NoteReview is the note of an entry no default entry matched: the settings are generic.
	NoteReview = "Migrated from the rates set on the model, with generic estimation settings (bytes, 3.0 bytes per token, largest output 131072). Review them."
	// NoteFilled is the note of an entry filled from a default entry; %s is replaced by its id.
	noteFilled = "Migrated from the rates set on the model; the other settings come from the default entry "
)

// NeedsReview reports whether an org entry still has generic migrated settings.
func NeedsReview(createdBy, note string) bool {
	return createdBy == MigrationActor && (note == NoteReview || strings.HasPrefix(note, NotePending))
}

// CompleteMigrated fills, once and in place, the org prices that migration 00014 made from model
// rates with what the old pricing took from the default entry that matched the model: cache write
// and reasoning rates, tokenizer and ratio, tool overhead, hidden thinking and max_output. Fields
// the model's rates set (input, output, cache read) stay. The default entry valid now is used. Tiers are not copied: the model's own
// rates applied at every context length. An entry no default entry matches keeps its generic
// settings and gets NoteReview, shown on the Prices page. It is idempotent and safe from every
// replica; it returns how many entries it completed.
func (s *Service) CompleteMigrated(ctx context.Context) (int, error) {
	q := store.New(s.pool)
	rows, err := q.ListPendingMigratedPrices(ctx)
	if err != nil {
		return 0, err
	}
	_, defs := pricing.Default()
	n := 0
	for _, r := range rows {
		var e pricing.Entry
		if err := json.Unmarshal(r.Entry, &e); err != nil {
			s.log.Warn("migrated price is unreadable; leaving it", "org", r.OrgID.String(), "price", r.Name, "err", err)
			continue
		}
		model := strings.TrimPrefix(e.Note, NotePending)
		e.Note = NoteReview
		if d, ok := pricing.Select(defs, e.Provider, model, time.Now()); ok {
			de := d.Entry
			if e.Prices.CacheWrite == "" {
				e.Prices.CacheWrite = de.Prices.CacheWrite
			}
			if e.Prices.Reasoning == "" {
				e.Prices.Reasoning = de.Prices.Reasoning
			}
			e.Tokenizer, e.Ratio = de.Tokenizer, de.Ratio
			e.ToolOverheadTokens, e.HiddenThinking = de.ToolOverheadTokens, de.HiddenThinking
			if de.MaxOutput > 0 {
				e.MaxOutput = de.MaxOutput
			}
			e.Note = noteFilled + de.ID
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return n, err
		}
		done, err := q.CompleteMigratedPrice(ctx, store.CompleteMigratedPriceParams{OrgID: r.OrgID, ID: r.ID, Entry: raw})
		if err != nil {
			return n, err
		}
		n += int(done)
	}
	if n > 0 {
		s.log.Info("completed the prices migrated from model rates", "prices", n, "at", time.Now().UTC().Format(time.RFC3339))
	}
	return n, nil
}
