// SPDX-License-Identifier: Apache-2.0

package executiontest

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"sync"
	"time"

	"github.com/stateracloud/koinon/internal/execution"
)

// Fake is an in-memory [execution.Compute] that also implements [execution.Suspender] and
// [execution.Snapshotter]. It is the reference for what a driver must do, and the driver for
// tests that need a backend and no gateway. Every sandbox is ready as soon as it is created.
// Exec ignores the command: the process copies its stdin to its stdout and exits with 0 when stdin
// ends. The zero value is not usable; call NewFake.
type Fake struct {
	mu         sync.Mutex
	seq        int
	workspaces map[string]execution.Workspace
	sandboxes  map[sbKey]*fakeSandbox
	snapshots  map[string]fakeSnapshot
}

type sbKey struct{ workspace, name string }

type fakeSandbox struct {
	sb    execution.Sandbox
	files map[string][]byte // absolute path to content
}

type fakeSnapshot struct {
	snap  execution.Snapshot
	files map[string][]byte
}

var (
	_ execution.Compute     = (*Fake)(nil)
	_ execution.Suspender   = (*Fake)(nil)
	_ execution.Snapshotter = (*Fake)(nil)
)

// NewFake returns an empty fake.
func NewFake() *Fake {
	return &Fake{
		workspaces: map[string]execution.Workspace{},
		sandboxes:  map[sbKey]*fakeSandbox{},
		snapshots:  map[string]fakeSnapshot{},
	}
}

func fail(op string, kind error, format string, args ...any) error {
	return &execution.Error{Op: op, Kind: kind, Err: fmt.Errorf(format, args...)}
}

// EnsureWorkspace implements [execution.Compute].
func (f *Fake) EnsureWorkspace(_ context.Context, name string, labels map[string]string) (execution.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if w, ok := f.workspaces[name]; ok {
		return w, nil
	}
	f.seq++
	w := execution.Workspace{Name: name, ID: fmt.Sprintf("ws-%d", f.seq), Labels: maps.Clone(labels), Active: true}
	f.workspaces[name] = w
	return w, nil
}

// get returns the sandbox or an ErrNotFound error. The caller holds f.mu.
func (f *Fake) get(op, workspace, name string) (*fakeSandbox, error) {
	s, ok := f.sandboxes[sbKey{workspace, name}]
	if !ok {
		return nil, fail(op, execution.ErrNotFound, "sandbox %q not found in workspace %q", name, workspace)
	}
	return s, nil
}

// CreateSandbox implements [execution.Compute].
func (f *Fake) CreateSandbox(_ context.Context, workspace string, spec execution.SandboxSpec) (execution.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.create("CreateSandbox", workspace, spec, nil)
}

func (f *Fake) create(op, workspace string, spec execution.SandboxSpec, files map[string][]byte) (execution.Sandbox, error) {
	if _, ok := f.workspaces[workspace]; !ok {
		return execution.Sandbox{}, fail(op, execution.ErrNotFound, "workspace %q not found", workspace)
	}
	if spec.Name == "" {
		return execution.Sandbox{}, fail(op, execution.ErrInvalid, "the sandbox needs a name")
	}
	if s, ok := f.sandboxes[sbKey{workspace, spec.Name}]; ok {
		return s.sb, nil
	}
	f.seq++
	sb := execution.Sandbox{
		Name: spec.Name, ID: fmt.Sprintf("sb-%d", f.seq), Workspace: workspace,
		Labels: maps.Clone(spec.Labels), Phase: execution.PhaseReady,
	}
	f.sandboxes[sbKey{workspace, spec.Name}] = &fakeSandbox{sb: sb, files: maps.Clone(files)}
	return sb, nil
}

// GetSandbox implements [execution.Compute].
func (f *Fake) GetSandbox(_ context.Context, workspace, name string) (execution.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.get("GetSandbox", workspace, name)
	if err != nil {
		return execution.Sandbox{}, err
	}
	return s.sb, nil
}

func (f *Fake) setPhase(op, workspace, name string, from []execution.Phase, to execution.Phase) (execution.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.get(op, workspace, name)
	if err != nil {
		return execution.Sandbox{}, err
	}
	if s.sb.Phase == to {
		return s.sb, nil
	}
	if !slices.Contains(from, s.sb.Phase) {
		return execution.Sandbox{}, fail(op, execution.ErrFailedPrecondition, "sandbox %q is %s", name, s.sb.Phase)
	}
	s.sb.Phase = to
	return s.sb, nil
}

// StartSandbox implements [execution.Compute].
func (f *Fake) StartSandbox(_ context.Context, workspace, name string) (execution.Sandbox, error) {
	return f.setPhase("StartSandbox", workspace, name, []execution.Phase{execution.PhaseStopped, execution.PhaseCompleted}, execution.PhaseReady)
}

// StopSandbox implements [execution.Compute].
func (f *Fake) StopSandbox(_ context.Context, workspace, name string) (execution.Sandbox, error) {
	return f.setPhase("StopSandbox", workspace, name, []execution.Phase{execution.PhaseReady, execution.PhaseSuspended, execution.PhaseCompleted}, execution.PhaseStopped)
}

// SuspendSandbox implements [execution.Suspender].
func (f *Fake) SuspendSandbox(_ context.Context, workspace, name string) (execution.Sandbox, error) {
	return f.setPhase("SuspendSandbox", workspace, name, []execution.Phase{execution.PhaseReady}, execution.PhaseSuspended)
}

// ResumeSandbox implements [execution.Suspender].
func (f *Fake) ResumeSandbox(_ context.Context, workspace, name string) (execution.Sandbox, error) {
	return f.setPhase("ResumeSandbox", workspace, name, []execution.Phase{execution.PhaseSuspended}, execution.PhaseReady)
}

// DeleteSandbox implements [execution.Compute]. The delete completes at once.
func (f *Fake) DeleteSandbox(_ context.Context, workspace, name string) (execution.DeleteOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sandboxes[sbKey{workspace, name}]; !ok {
		return execution.DeleteAbsent, nil
	}
	delete(f.sandboxes, sbKey{workspace, name})
	return execution.DeleteCompleted, nil
}

// ready returns the sandbox if it is ready. The caller holds f.mu.
func (f *Fake) ready(op, workspace, name string) (*fakeSandbox, error) {
	s, err := f.get(op, workspace, name)
	if err != nil {
		return nil, err
	}
	if s.sb.Phase != execution.PhaseReady {
		return nil, fail(op, execution.ErrFailedPrecondition, "sandbox %q is not ready (%s)", name, s.sb.Phase)
	}
	return s, nil
}

// Exec implements [execution.Compute].
func (f *Fake) Exec(ctx context.Context, workspace, sandbox string, req execution.ExecRequest) (execution.Process, error) {
	f.mu.Lock()
	_, err := f.ready("Exec", workspace, sandbox)
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if len(req.Command) == 0 {
		return nil, fail("Exec", execution.ErrInvalid, "the command is empty")
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := &fakeProcess{stdin: inW, stdout: outR, done: make(chan struct{}), closeAll: func() {
		_ = inR.Close()
		_ = outR.Close()
	}}
	go func() {
		_, err := io.Copy(outW, inR)
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		_ = outW.Close()
		close(p.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Close()
		case <-p.done:
		}
	}()
	return p, nil
}

type fakeProcess struct {
	stdin    *io.PipeWriter
	stdout   *io.PipeReader
	done     chan struct{}
	closeAll func()
	mu       sync.Mutex
	err      error
	closed   bool
}

func (p *fakeProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *fakeProcess) Stdout() io.Reader     { return p.stdout }

func (p *fakeProcess) Wait() (int, error) {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.closed:
		return 0, execution.ErrStreamClosed
	case p.err != nil && !errors.Is(p.err, io.ErrClosedPipe):
		return 0, p.err
	}
	return 0, nil
}

func (p *fakeProcess) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.closeAll()
	_ = p.stdin.Close()
	return nil
}

// Upload implements [execution.Compute]. It keeps the regular files of the archive.
func (f *Fake) Upload(_ context.Context, workspace, sandbox, destDir string, archive io.Reader) error {
	f.mu.Lock()
	_, err := f.ready("Upload", workspace, sandbox)
	f.mu.Unlock()
	if err != nil {
		return err
	}
	got := map[string][]byte{}
	tr := tar.NewReader(archive)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail("Upload", execution.ErrInvalid, "read the archive: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return fail("Upload", execution.ErrInvalid, "read the archive: %w", err)
		}
		got[path.Join(destDir, h.Name)] = b
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.ready("Upload", workspace, sandbox)
	if err != nil {
		return err
	}
	if s.files == nil {
		s.files = map[string][]byte{}
	}
	maps.Copy(s.files, got)
	return nil
}

// File returns the content of an uploaded file, by absolute path.
func (f *Fake) File(workspace, sandbox, p string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sandboxes[sbKey{workspace, sandbox}]
	if !ok {
		return nil, false
	}
	b, ok := s.files[p]
	return slices.Clone(b), ok
}

// digest is the sha256 of the files in path order, each as its path, its length and its bytes.
func digest(files map[string][]byte) string {
	h := sha256.New()
	for _, p := range slices.Sorted(maps.Keys(files)) {
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00", p, len(files[p]))
		h.Write(files[p])
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// SnapshotSandbox implements [execution.Snapshotter]. A snapshot holds the disk.
func (f *Fake) SnapshotSandbox(_ context.Context, workspace, name string) (execution.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.get("SnapshotSandbox", workspace, name)
	if err != nil {
		return execution.Snapshot{}, err
	}
	f.seq++
	files := cloneFiles(s.files)
	snap := execution.Snapshot{
		SnapshotRef: execution.SnapshotRef{ID: fmt.Sprintf("snap-%d", f.seq), Digest: digest(files)},
		Kind:        execution.SnapshotDisk, Workspace: workspace, Sandbox: name, CreatedAt: time.Now().UTC(),
	}
	f.snapshots[snap.ID] = fakeSnapshot{snap: snap, files: files}
	return snap, nil
}

// RestoreSandbox implements [execution.Snapshotter].
func (f *Fake) RestoreSandbox(_ context.Context, workspace string, ref execution.SnapshotRef, spec execution.SandboxSpec) (execution.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	saved, ok := f.snapshots[ref.ID]
	if !ok {
		return execution.Sandbox{}, fail("RestoreSandbox", execution.ErrNotFound, "snapshot %q not found", ref.ID)
	}
	if ref.Digest != saved.snap.Digest || digest(saved.files) != saved.snap.Digest {
		return execution.Sandbox{}, fail("RestoreSandbox", execution.ErrInvalid, "snapshot %q does not match digest %q", ref.ID, ref.Digest)
	}
	if _, ok := f.sandboxes[sbKey{workspace, spec.Name}]; ok {
		return execution.Sandbox{}, fail("RestoreSandbox", execution.ErrAlreadyExists, "sandbox %q exists", spec.Name)
	}
	return f.create("RestoreSandbox", workspace, spec, cloneFiles(saved.files))
}

func cloneFiles(in map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(in))
	for k, v := range in {
		out[k] = slices.Clone(v)
	}
	return out
}
