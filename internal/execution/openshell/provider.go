// SPDX-License-Identifier: Apache-2.0

package openshell

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/stateracloud/koinon/internal/execution"
	datamodelv1 "github.com/stateracloud/koinon/internal/gen/openshell/datamodel/v1"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
	"github.com/stateracloud/koinon/internal/project"
)

// Keys Koinon stores on OpenShell records to make "ensure" cheap.
var (
	// SpecHashAnnotation on a provider profile holds the hash of the profile as Koinon
	// rendered it, so an unchanged profile is not rewritten.
	SpecHashAnnotation = project.Domain + "/spec-hash"
	// CredentialRevisionConfig in a provider's config holds Provider.CredentialRevision.
	// It lives in config because OpenShell keeps provider metadata immutable after
	// creation, while config is merged on update.
	CredentialRevisionConfig = project.Domain + "/credential-revision"
)

const profileSource = "koinon"

// EnsureProvider implements execution.Backend.
func (b *Backend) EnsureProvider(ctx context.Context, workspace string, p execution.Provider) error {
	const op = "EnsureProvider"
	if p.Name == "" {
		return &execution.Error{Op: op, Kind: execution.ErrInvalid, Err: errors.New("provider needs a name")}
	}
	if err := b.ensureProfile(ctx, workspace, p.Profile); err != nil {
		return err
	}

	config := maps.Clone(p.Config)
	if config == nil {
		config = map[string]string{}
	}
	if p.CredentialRevision != "" {
		config[CredentialRevisionConfig] = p.CredentialRevision
	}

	create := func() error {
		_, err := b.c.CreateProvider(ctx, &openshellv1.CreateProviderRequest{
			WorkspaceScope: scope(workspace),
			Provider: &datamodelv1.Provider{
				Metadata:         &datamodelv1.ObjectMeta{Name: p.Name, Labels: b.stamp(p.Labels)},
				Type:             p.Profile.ID,
				Credentials:      maps.Clone(p.Credentials),
				Config:           config,
				ProfileWorkspace: workspace,
			},
		})
		if errors.Is(wrap(op, err), execution.ErrAlreadyExists) {
			// Created concurrently; the next reconcile compares it.
			return &execution.Error{Op: op, Kind: execution.ErrConflict, Err: err}
		}
		return wrap(op, err)
	}

	got, err := b.c.GetProvider(ctx, &openshellv1.GetProviderRequest{WorkspaceScope: scope(workspace), Name: p.Name})
	switch {
	case err == nil:
	case errors.Is(wrap(op, err), execution.ErrNotFound):
		return create()
	default:
		return wrap(op, err)
	}

	existing := got.GetProvider()
	if err := b.check(op, "provider", p.Name, existing.GetMetadata().GetLabels()); err != nil {
		return err
	}
	legacy := b.unlabeled(existing.GetMetadata().GetLabels())
	if p.ClaimLegacy && legacy {
		// Made before the install label existed (issue 461). Labels are immutable, so claim it by
		// recreating it with the label and the credentials of this pass. The caller sets ClaimLegacy
		// only while no sandbox attaches it; until then the record is taken as this install's.
		if _, err := b.c.DeleteProvider(ctx, &openshellv1.DeleteProviderRequest{
			WorkspaceScope: scope(workspace), Name: p.Name, AllowMissing: true,
		}); err != nil {
			return wrap(op, err)
		}
		if err := create(); err != nil {
			return err
		}
		return nil
	}
	if existing.GetType() != p.Profile.ID || existing.GetProfileWorkspace() != workspace {
		return &execution.Error{Op: op, Kind: execution.ErrConflict, Err: fmt.Errorf(
			"provider %q has type %q in profile workspace %q; OpenShell cannot change either, delete and recreate it",
			p.Name, existing.GetType(), existing.GetProfileWorkspace())}
	}

	update := &datamodelv1.Provider{Metadata: &datamodelv1.ObjectMeta{Name: p.Name}}
	if existing.GetConfig()[CredentialRevisionConfig] != p.CredentialRevision {
		update.Credentials = maps.Clone(p.Credentials)
	}
	// OpenShell merges config on update; an empty value removes a key.
	changed := map[string]string{}
	for k, v := range config {
		if existing.GetConfig()[k] != v {
			changed[k] = v
		}
	}
	for k := range existing.GetConfig() {
		if _, keep := config[k]; !keep {
			changed[k] = ""
		}
	}
	if len(changed) == 0 && update.Credentials == nil {
		return nil
	}
	update.Config = changed
	_, err = b.c.UpdateProvider(ctx, &openshellv1.UpdateProviderRequest{
		WorkspaceScope: scope(workspace),
		Provider:       update,
	})
	return wrap(op, err)
}

// ensureProfile imports the workspace-scoped profile, or updates it when the hash
// recorded on it differs from the rendering.
func (b *Backend) ensureProfile(ctx context.Context, workspace string, p execution.ProviderProfile) error {
	const op = "EnsureProviderProfile"
	b.profileMu.Lock()
	defer b.profileMu.Unlock()
	profile, err := renderProfile(p)
	if err != nil {
		return &execution.Error{Op: op, Kind: execution.ErrInvalid, Err: err}
	}
	hash, err := specHash(profile)
	if err != nil {
		return &execution.Error{Op: op, Err: err}
	}
	if b.owner != "" {
		// The owner is part of the hash, so a profile made before the install annotation existed
		// differs and is re-imported with it (issue 461).
		sum := sha256.Sum256([]byte(hash + "|" + b.owner))
		hash = hex.EncodeToString(sum[:])
	}
	profile.Annotations = b.stamp(map[string]string{SpecHashAnnotation: hash})
	item := &openshellv1.ProviderProfileImportItem{Profile: profile, Source: profileSource}

	got, err := b.c.GetProviderProfile(ctx, &openshellv1.GetProviderProfileRequest{WorkspaceScope: scope(workspace), Id: p.ID})
	found := err == nil
	if err != nil && !errors.Is(wrap(op, err), execution.ErrNotFound) {
		return wrap(op, err)
	}
	if found {
		if err := b.check(op, "provider profile", p.ID, got.GetProfile().GetAnnotations()); err != nil {
			return err
		}
	}
	if found && got.GetProfile().GetAnnotations()[SpecHashAnnotation] == hash {
		return nil
	}

	// Older gateways silently drop unknown credential fields, so check that this one keeps
	// stable_placeholder before writing.
	lint, err := b.c.LintProviderProfiles(ctx, &openshellv1.LintProviderProfilesRequest{
		WorkspaceScope: scope(workspace),
		Profiles:       []*openshellv1.ProviderProfileImportItem{item},
	})
	if err != nil {
		return wrap(op, err)
	}
	if !lint.GetSupportsStablePlaceholder() {
		return &execution.Error{Op: op, Kind: execution.ErrUnsupported, Err: errors.New("gateway does not support stable_placeholder")}
	}
	// Lint treats the request as an import, so it reports an id conflict for every profile that
	// already exists. An update of a found profile must not fail on that one diagnostic;
	// UpdateProviderProfiles validates on its own (issue 508).
	if blocking := lintBlocking(lint.GetDiagnostics(), found, p.ID); len(blocking) > 0 {
		return &execution.Error{Op: op, Kind: execution.ErrInvalid, Err: diagnostics(blocking)}
	}

	if !found {
		resp, err := b.c.ImportProviderProfiles(ctx, &openshellv1.ImportProviderProfilesRequest{
			WorkspaceScope: scope(workspace),
			Profiles:       []*openshellv1.ProviderProfileImportItem{item},
		})
		if err != nil {
			return wrap(op, err)
		}
		if !resp.GetImported() {
			return &execution.Error{Op: op, Kind: execution.ErrInvalid, Err: diagnostics(resp.GetDiagnostics())}
		}
		return nil
	}
	resp, err := b.c.UpdateProviderProfiles(ctx, &openshellv1.UpdateProviderProfilesRequest{
		WorkspaceScope:          scope(workspace),
		Id:                      p.ID,
		Profile:                 item,
		ExpectedResourceVersion: got.GetProfile().GetResourceVersion(),
	})
	if err != nil {
		return wrap(op, err)
	}
	if !resp.GetUpdated() {
		return &execution.Error{Op: op, Kind: execution.ErrInvalid, Err: diagnostics(resp.GetDiagnostics())}
	}
	return nil
}

// lintBlocking returns the error diagnostics that stop a write. When the profile exists, the
// "id already exists" error for id is expected and dropped.
func lintBlocking(ds []*openshellv1.ProviderProfileDiagnostic, exists bool, id string) []*openshellv1.ProviderProfileDiagnostic {
	var out []*openshellv1.ProviderProfileDiagnostic
	for _, d := range ds {
		if d.GetSeverity() != "error" {
			continue
		}
		if exists && d.GetField() == "id" && d.GetProfileId() == id && strings.Contains(d.GetMessage(), "already exists") {
			continue
		}
		out = append(out, d)
	}
	return out
}

func specHash(m proto.Message) (string, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func diagnostics(ds []*openshellv1.ProviderProfileDiagnostic) error {
	if len(ds) == 0 {
		return errors.New("rejected without diagnostics")
	}
	msgs := make([]string, 0, len(ds))
	for _, d := range ds {
		msgs = append(msgs, fmt.Sprintf("%s %s: %s", d.GetSeverity(), d.GetField(), d.GetMessage()))
	}
	return errors.New(strings.Join(msgs, "; "))
}
