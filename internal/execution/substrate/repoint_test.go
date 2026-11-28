// SPDX-License-Identifier: Apache-2.0

package substrate_test

import (
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/execution/substrate/fakeate"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
	"github.com/stateracloud/koinon/internal/harness"
)

const img2 = "img2" + digest

// Issue #1482, invariant 3: the template keeps the container name, the volumes, the mounts and the
// storage location. On gVisor the checkpoint of the home is keyed by the container name, so a change
// of any of them empties the home with no error from Substrate. These specs fail when one changes.
var _ = Describe("the template invariants that keep the home (issue #1482)", func() {
	cfg := substrate.Config{
		GuestSigner:             fakeate.Signer(),
		SnapshotStorageLocation: "snapshots",
		DefaultTier:             harness.TierDev,
		RuntimeClassTiers:       map[string]harness.Tier{"kata": harness.TierVM, "gvisor": harness.TierHardened},
	}
	render := func(class string) *ateapiv1.ActorTemplate {
		t, err := substrate.RenderTemplateForTest(ws, execution.SandboxSpec{Name: "a", Image: img, CPULimit: "1", MemoryLimit: "1Gi", RuntimeClassName: class}, cfg)
		Expect(err).NotTo(HaveOccurred())
		return t
	}

	It("renders the golden container name, volumes, mounts and storage location on every tier", func() {
		for _, class := range []string{"", "kata", "gvisor"} {
			t := render(class)
			Expect(t.GetContainers()).To(HaveLen(1), class)
			Expect(t.GetContainers()[0].GetName()).To(Equal("agent"), class)
			Expect(t.GetContainers()[0].GetVolumeMounts()).To(HaveLen(1), class)
			Expect(t.GetContainers()[0].GetVolumeMounts()[0].GetName()).To(Equal("home"), class)
			Expect(t.GetContainers()[0].GetVolumeMounts()[0].GetMountPath()).To(Equal("/sandbox"), class)
			Expect(t.GetVolumes()).To(HaveLen(1), class)
			Expect(t.GetVolumes()[0].GetName()).To(Equal("home"), class)
			Expect(t.GetVolumes()[0].GetDurableDir()).NotTo(BeNil(), class)
			Expect(t.GetSnapshotConfig().GetStorageLocation()).To(Equal("snapshots"), class)
		}
	})

	It("lets an image change through", func() {
		next, err := substrate.RenderTemplateForTest(ws, execution.SandboxSpec{Name: "a", Image: img2, CPULimit: "2", MemoryLimit: "1Gi"}, cfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(substrate.RepointInvariants(render(""), next, cfg)).To(Succeed())
	})

	DescribeTable("refuses a change that would empty or lose the home",
		func(change func(*ateapiv1.ActorTemplate), why string) {
			next := proto.Clone(render("")).(*ateapiv1.ActorTemplate)
			change(next)
			Expect(substrate.RepointInvariants(render(""), next, cfg)).To(MatchError(ContainSubstring(why)))
		},
		Entry("a renamed container", func(t *ateapiv1.ActorTemplate) { t.Containers[0].Name = "main" }, "named"),
		Entry("a second container", func(t *ateapiv1.ActorTemplate) {
			t.Containers = append(t.Containers, &ateapiv1.Container{Name: "side"})
		}, "containers"),
		Entry("another mount path", func(t *ateapiv1.ActorTemplate) { t.Containers[0].VolumeMounts[0].MountPath = "/home/agent" }, "mounts"),
		Entry("a removed mount", func(t *ateapiv1.ActorTemplate) { t.Containers[0].VolumeMounts = nil }, "mounts"),
		Entry("a renamed volume", func(t *ateapiv1.ActorTemplate) { t.Volumes[0].Name = "data" }, "volumes"),
		Entry("a volume of another kind", func(t *ateapiv1.ActorTemplate) { t.Volumes[0].DurableDir = nil }, "volumes"),
		Entry("another storage location", func(t *ateapiv1.ActorTemplate) { t.SnapshotConfig.StorageLocation = "other" }, "storage location"),
	)

	It("refuses a tier change", func() {
		Expect(substrate.RepointInvariants(render(""), render("kata"), cfg)).To(MatchError(ContainSubstring("tier")))
		Expect(substrate.RepointInvariants(render("gvisor"), render("kata"), cfg)).To(MatchError(ContainSubstring("tier")))
		Expect(substrate.RepointInvariants(render("kata"), render("kata"), cfg)).To(Succeed())
	})
})

var _ = Describe("Substrate repoint of a sandbox (issue #1482)", func() {
	const home = "/sandbox/notes.md"

	// started returns a driver with a sandbox "a" that has a file in its home and is suspended.
	started := func(ctx SpecContext) (*substrate.Driver, *fakeate.Cluster) {
		d, cl := newDriver()
		ready(ctx, d, "a")
		cl.PutFile(ws, "a", home, []byte("kept"))
		_, err := d.SuspendSandbox(ctx, ws, "a")
		Expect(err).NotTo(HaveOccurred())
		return d, cl
	}
	changed := func() execution.SandboxSpec {
		s := spec("a")
		s.Image = img2
		return s
	}

	It("keeps the home of a suspended sandbox, resumes it on the new template, and never deletes a template", func(ctx SpecContext) {
		d, cl := started(ctx)
		old := cl.TemplateOf(ws, "a")
		sb, err := d.RepointSandbox(ctx, ws, "a", changed())
		Expect(err).NotTo(HaveOccurred())
		Expect(sb.SpecID).NotTo(Equal(old))
		Expect(cl.TemplateOf(ws, "a")).To(Equal(sb.SpecID))
		want, err := d.SpecID(ws, changed())
		Expect(err).NotTo(HaveOccurred())
		Expect(sb.SpecID).To(Equal(want))
		_, err = d.WaitPhase(ctx, ws, "a", execution.PhaseReady)
		Expect(err).NotTo(HaveOccurred())
		body, ok := cl.File(ws, "a", home)
		Expect(ok).To(BeTrue())
		Expect(string(body)).To(Equal("kept"))
		Expect(cl.Templates(ws)).To(ContainElement(old), "the old template stays: other actors can name it")
		Expect(cl.Calls("DeleteActorTemplate")).To(BeZero())
		Expect(cl.Calls("DeleteActor")).To(BeZero())
		Expect(cl.Calls("ResumeWithoutEgressPolicy")).To(BeZero())
	})

	It("suspends a running sandbox with a snapshot first", func(ctx SpecContext) {
		d, cl := newDriver()
		ready(ctx, d, "a")
		cl.PutFile(ws, "a", home, []byte("kept"))
		before := cl.Calls("SuspendActor")
		_, err := d.RepointSandbox(ctx, ws, "a", changed())
		Expect(err).NotTo(HaveOccurred())
		Expect(cl.Calls("SuspendActor")).To(BeNumerically(">", before))
		Expect(cl.TemplateOf(ws, "a")).NotTo(BeEmpty())
		body, _ := cl.File(ws, "a", home)
		Expect(string(body)).To(Equal("kept"))
	})

	It("takes a new snapshot after the repoint, so a revert does not go back to the old template", func(ctx SpecContext) {
		d, cl := started(ctx)
		before := cl.Calls("SuspendActor")
		sb, err := d.RepointSandbox(ctx, ws, "a", changed())
		Expect(err).NotTo(HaveOccurred())
		Expect(cl.Calls("SuspendActor")).To(Equal(before+1), "one seal after the repoint")
		Expect(sb.NewSnapshot).To(BeTrue())
	})

	It("does nothing for a sandbox that names the template already", func(ctx SpecContext) {
		d, cl := started(ctx)
		_, err := d.RepointSandbox(ctx, ws, "a", spec("a"))
		Expect(err).NotTo(HaveOccurred())
		Expect(cl.Calls("UpdateActor")).To(BeZero())
	})

	It("refuses and changes nothing when Substrate refuses UpdateActor", func(ctx SpecContext) {
		for _, code := range []codes.Code{codes.FailedPrecondition, codes.InvalidArgument} {
			d, cl := started(ctx)
			old := cl.TemplateOf(ws, "a")
			cl.RefuseNextUpdateActor(ws, "a", code)
			_, err := d.RepointSandbox(ctx, ws, "a", changed())
			Expect(errors.Is(err, execution.ErrRepointRefused)).To(BeTrue(), "%v", err)
			Expect(cl.TemplateOf(ws, "a")).To(Equal(old))
			Expect(cl.State(ws, "a")).To(Equal(ateapiv1.ActorState_ACTOR_STATE_SUSPENDED))
			body, _ := cl.File(ws, "a", home)
			Expect(string(body)).To(Equal("kept"))
		}
	})

	It("refuses a tier change before it calls UpdateActor", func(ctx SpecContext) {
		d, cl := started(ctx)
		s := changed()
		s.RuntimeClassName = "kata"
		_, err := d.RepointSandbox(ctx, ws, "a", s)
		Expect(errors.Is(err, execution.ErrRepointRefused)).To(BeTrue(), "%v", err)
		Expect(cl.Calls("UpdateActor")).To(BeZero())
	})

	It("refuses when the template of the sandbox is gone", func(ctx SpecContext) {
		d, cl := started(ctx)
		_, err := lastControl.DeleteActorTemplate(ctx, &ateapiv1.DeleteActorTemplateRequest{ActorTemplate: &ateapiv1.ObjectRef{Atespace: ws, Name: cl.TemplateOf(ws, "a")}})
		Expect(err).NotTo(HaveOccurred())
		_, err = d.RepointSandbox(ctx, ws, "a", changed())
		Expect(errors.Is(err, execution.ErrRepointRefused)).To(BeTrue(), "%v", err)
	})

	It("reports ErrRepointResume, and keeps the sandbox, when the resume fails after the repoint", func(ctx SpecContext) {
		d, cl := started(ctx)
		cl.FailNextResumes(ws, "a", 1000)
		_, err := d.RepointSandbox(ctx, ws, "a", changed())
		Expect(errors.Is(err, execution.ErrRepointResume)).To(BeTrue(), "%v", err)
		want, serr := d.SpecID(ws, changed())
		Expect(serr).NotTo(HaveOccurred())
		Expect(cl.TemplateOf(ws, "a")).To(Equal(want))
		Expect(cl.Calls("DeleteActor")).To(BeZero())
		body, _ := cl.File(ws, "a", home)
		Expect(string(body)).To(Equal("kept"))
	})

	It("fails for a sandbox that is not there", func(ctx SpecContext) {
		d, _ := newDriver()
		_, err := d.RepointSandbox(ctx, ws, "none", changed())
		Expect(errors.Is(err, execution.ErrNotFound)).To(BeTrue(), "%v", err)
	})

	Describe("DeleteUnusedTemplate", func() {
		It("never deletes a template that an actor names, and deletes one that none names", func(ctx SpecContext) {
			d, cl := started(ctx)
			used := cl.TemplateOf(ws, "a")
			err := d.DeleteUnusedTemplate(ctx, ws, used)
			Expect(errors.Is(err, execution.ErrFailedPrecondition)).To(BeTrue(), "%v", err)
			Expect(cl.Calls("DeleteActorTemplate")).To(BeZero())

			_, err = d.RepointSandbox(ctx, ws, "a", changed())
			Expect(err).NotTo(HaveOccurred())
			Expect(d.DeleteUnusedTemplate(ctx, ws, used)).To(Succeed())
			Expect(cl.Templates(ws)).NotTo(ContainElement(used))
			Expect(d.DeleteUnusedTemplate(ctx, ws, cl.TemplateOf(ws, "a"))).NotTo(Succeed(), "the new template is in use")
		})

		It("refuses a template that the driver did not make", func(ctx SpecContext) {
			d, _ := newDriver()
			Expect(errors.Is(d.DeleteUnusedTemplate(ctx, ws, "hand-made"), execution.ErrInvalid)).To(BeTrue())
		})
	})
})
