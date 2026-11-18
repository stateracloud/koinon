// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/execution"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
)

// snapshotPrefix starts the name of every tag the driver makes.
const snapshotPrefix = "ks-"

// digestVersion is the first line of the digested text. A change to what the digest covers changes
// it, so an old digest never matches a new computation by accident.
const digestVersion = "koinon-substrate-snapshot/v1"

// SnapshotSandbox implements execution.Snapshotter.
//
// Substrate tags only a suspended actor ("tag the external snapshot a suspended actor holds"), so a
// running sandbox is suspended with synchronous durability, tagged, and resumed. The driver's own
// streams stop before the suspend and read again after the resume (see Exec), and the sandbox is
// back to running when the call returns, as the seam says. With memory fidelity (D5, the default)
// the snapshot holds the whole guest, so a clone of it holds whatever the guest held: the guest
// must hold no credential, which ADR 0119 makes an invariant of the image and the launcher, and the
// driver puts none in a template.
//
// The digest is Koinon's own record, because Substrate gives none before restore (upstream T-25):
// it covers the object's bytes through [Config.ContentDigest], and the call fails closed without it.
func (d *Driver) SnapshotSandbox(ctx context.Context, workspace, name string) (execution.Snapshot, error) {
	const op = "SnapshotSandbox"
	k := key{workspace, name}
	if err := d.requireContentDigest(op); err != nil {
		return execution.Snapshot{}, err
	}
	actor, err := d.control.GetActor(ctx, &ateapiv1.GetActorRequest{Actor: ref(workspace, name)})
	if err != nil {
		return execution.Snapshot{}, execution.GRPCError(op, err)
	}
	wasRunning := false
	switch actor.GetStatus().GetState() {
	case ateapiv1.ActorState_ACTOR_STATE_RUNNING:
		wasRunning = true
		if _, err := d.suspend(ctx, op, workspace, name); err != nil {
			return execution.Snapshot{}, err
		}
	case ateapiv1.ActorState_ACTOR_STATE_SUSPENDED:
		// An earlier snapshot of a running sandbox could not resume it: this one owes the resume.
		wasRunning = d.isOwed(k)
	default:
		return execution.Snapshot{}, fail(op, execution.ErrFailedPrecondition, "sandbox %s is %s, not ready or suspended", k, phaseOf(actor))
	}

	tag, tagErr := d.tag(ctx, workspace, name)
	if wasRunning { // resume even when the tag failed: the sandbox keeps running
		if _, rerr := d.resume(context.WithoutCancel(ctx), op, workspace, name); rerr != nil {
			// Remember the debt: a retry finds the actor suspended and must still resume it.
			d.setOwed(k, true)
			if tagErr == nil {
				return execution.Snapshot{}, rerr
			}
		}
	}
	if tagErr != nil {
		return execution.Snapshot{}, tagErr
	}
	// Read the tag back, so the digest comes from the same read as a restore will make.
	tag, err = d.control.GetTag(ctx, &ateapiv1.GetTagRequest{Tag: ref(workspace, tag.GetMetadata().GetName())})
	if err != nil {
		return execution.Snapshot{}, execution.GRPCError(op, err)
	}
	digest, err := d.digest(ctx, tag)
	if err != nil {
		return execution.Snapshot{}, execution.GRPCError(op, err)
	}
	return snapshotFrom(workspace, tag, digest), nil
}

func (d *Driver) tag(ctx context.Context, workspace, source string) (*ateapiv1.Tag, error) {
	const op = "SnapshotSandbox"
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, fmt.Errorf("substrate: %w", err)
	}
	tag, err := d.control.CreateTag(ctx, &ateapiv1.CreateTagRequest{Tag: &ateapiv1.Tag{
		Metadata:    &ateapiv1.ResourceMetadata{Atespace: workspace, Name: snapshotPrefix + hex.EncodeToString(rnd[:])},
		Scope:       ateapiv1.TagScope_TAG_SCOPE_ATESPACE, // a clone stays in the org's atespace (ADR 0002)
		SourceActor: ref(workspace, source),
	}})
	if err != nil {
		return nil, execution.GRPCError(op, err)
	}
	if tag.GetStatus().GetSnapshot().GetDurableSnapshot().GetState() != ateapiv1.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED {
		return nil, fail(op, execution.ErrUnavailable, "snapshot %s is not stored yet", tag.GetMetadata().GetName())
	}
	return tag, nil
}

// RestoreSandbox implements execution.Snapshotter. It reads the tag, recomputes its digest and
// refuses the restore when it differs from ref.Digest (execution.ErrInvalid), before it creates
// anything. The new actor takes the snapshot's own template, because Substrate seeds an actor only
// from a tag of the same template: spec supplies the name, and its image and resources must be those
// of the source (the reconciler restores a sandbox of the same agent version). The restored actor
// gets a fresh identity: its token is minted by Substrate for the new actor, outside the guest, and
// the template holds no environment variable, so the clone shares no secret with its source.
func (d *Driver) RestoreSandbox(ctx context.Context, workspace string, snap execution.SnapshotRef, spec execution.SandboxSpec) (execution.Sandbox, error) {
	const op = "RestoreSandbox"
	if err := d.requireContentDigest(op); err != nil {
		return execution.Sandbox{}, err
	}
	if spec.Name == "" {
		return execution.Sandbox{}, fail(op, execution.ErrInvalid, "sandbox name is empty")
	}
	if err := checkSpec(op, spec); err != nil {
		return execution.Sandbox{}, err
	}
	tag, err := d.control.GetTag(ctx, &ateapiv1.GetTagRequest{Tag: ref(workspace, snap.ID)})
	if err != nil {
		return execution.Sandbox{}, execution.GRPCError(op, err)
	}
	digest, err := d.digest(ctx, tag)
	if err != nil {
		return execution.Sandbox{}, execution.GRPCError(op, err)
	}
	if digest != snap.Digest {
		return execution.Sandbox{}, fail(op, execution.ErrInvalid, "snapshot %s does not match its recorded digest", snap.ID)
	}
	if tag.GetStatus().GetSnapshot().GetDurableSnapshot().GetState() != ateapiv1.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED {
		return execution.Sandbox{}, fail(op, execution.ErrFailedPrecondition, "snapshot %s is not stored", snap.ID)
	}
	tmpl, err := d.templateByUID(ctx, workspace, tag.GetStatus().GetActorTemplateUid())
	if err != nil {
		return execution.Sandbox{}, err
	}
	actor, err := d.control.CreateActor(ctx, &ateapiv1.CreateActorRequest{Actor: &ateapiv1.Actor{
		Metadata:      &ateapiv1.ResourceMetadata{Atespace: workspace, Name: spec.Name},
		ActorTemplate: ref(workspace, tmpl),
		SourceTag:     ref(workspace, snap.ID),
	}})
	if err != nil {
		return execution.Sandbox{}, execution.GRPCError(op, err)
	}
	return sandboxFrom(workspace, actor), nil
}

// templateByUID finds the template a tag was taken under.
func (d *Driver) templateByUID(ctx context.Context, workspace, uid string) (string, error) {
	const op = "RestoreSandbox"
	page := ""
	for {
		resp, err := d.control.ListActorTemplates(ctx, &ateapiv1.ListActorTemplatesRequest{
			Atespace: workspace, Options: &ateapiv1.ListOptions{PageSize: 100, PageToken: page},
		})
		if err != nil {
			return "", execution.GRPCError(op, err)
		}
		for _, t := range resp.GetActorTemplates() {
			if t.GetMetadata().GetUid() == uid {
				return t.GetMetadata().GetName(), nil
			}
		}
		if page = resp.GetNextPageToken(); page == "" {
			return "", fail(op, execution.ErrNotFound, "the template of the snapshot (%s) no longer exists", uid)
		}
	}
}

// requireContentDigest fails closed when no object-content digest is configured: see
// [Config.ContentDigest].
func (d *Driver) requireContentDigest(op string) error {
	if d.cfg.ContentDigest == nil {
		return fail(op, execution.ErrUnsupported,
			"snapshots need Config.ContentDigest: a digest over metadata alone would restore a changed object (ADR 0119 D5)")
	}
	return nil
}

// digest is "sha256:<hex>" over the text that identifies a snapshot: where it lives, what it was
// taken from and what its object holds ([Config.ContentDigest]). Anything the control
// API reports differently on a later read (a swapped object address, another generation or
// fidelity, a re-pointed tag) changes it.
func (d *Driver) digest(ctx context.Context, tag *ateapiv1.Tag) (string, error) {
	s := tag.GetStatus().GetSnapshot()
	uri := s.GetDurableSnapshot().GetObject().GetSnapshotUri()
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\natespace=%s\ntag=%s\ntag_uid=%s\nsource=%s\nsnapshot=%s\ngeneration=%d\nfidelity=%s\ntemplate=%s\nobject=%s\n",
		digestVersion, tag.GetMetadata().GetAtespace(), tag.GetMetadata().GetName(), tag.GetMetadata().GetUid(),
		tag.GetSourceActor().GetName(), s.GetUuid(), s.GetGeneration(), s.GetFidelity(),
		tag.GetStatus().GetActorTemplateUid(), uri)
	c, err := d.cfg.ContentDigest(ctx, uri)
	if err != nil {
		return "", err
	}
	_, _ = fmt.Fprintf(h, "content=%s\n", c)
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func snapshotFrom(workspace string, tag *ateapiv1.Tag, digest string) execution.Snapshot {
	kind := execution.SnapshotDisk
	if tag.GetStatus().GetSnapshot().GetFidelity() == ateapiv1.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY {
		kind = execution.SnapshotDiskMemory
	}
	var created time.Time
	if t := tag.GetMetadata().GetCreateTime(); t != nil {
		created = t.AsTime()
	}
	return execution.Snapshot{
		SnapshotRef: execution.SnapshotRef{ID: tag.GetMetadata().GetName(), Digest: digest},
		Kind:        kind,
		Workspace:   workspace,
		Sandbox:     tag.GetSourceActor().GetName(),
		CreatedAt:   created,
	}
}

// IsSnapshotTag reports whether a tag name was made by the driver.
func IsSnapshotTag(name string) bool { return strings.HasPrefix(name, snapshotPrefix) }
