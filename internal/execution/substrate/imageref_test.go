// SPDX-License-Identifier: Apache-2.0

package substrate_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/execution/substrate/fakeate"
)

const digest = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var _ = Describe("RewriteImage", func() {
	mirrors := map[string]string{
		"ghcr.io":   "cache.koinon.svc/ghcr.io",
		"docker.io": "cache.koinon.svc/docker.io/",
	}

	DescribeTable("rewrites the host and keeps the path, the tag and the digest",
		func(ref, registry string, m map[string]string, want string) {
			Expect(substrate.RewriteImage(ref, registry, m)).To(Equal(want))
		},
		Entry("a mirror host", "ghcr.io/stateracloud/koinon/agent:1"+digest, "", mirrors,
			"cache.koinon.svc/ghcr.io/stateracloud/koinon/agent:1"+digest),
		Entry("the host name is not case sensitive", "GHCR.io/org/app:1", "", mirrors, "cache.koinon.svc/ghcr.io/org/app:1"),
		Entry("a bare name is docker.io/library", "busybox:1.36", "", mirrors, "cache.koinon.svc/docker.io/library/busybox:1.36"),
		Entry("docker.io with one path element gets library", "docker.io/alpine:3", "", mirrors, "cache.koinon.svc/docker.io/library/alpine:3"),
		Entry("index.docker.io with one path element gets library", "index.docker.io/alpine:3", "", mirrors, "cache.koinon.svc/docker.io/library/alpine:3"),
		Entry("user/name is on docker.io", "acme/tool:2", "", mirrors, "cache.koinon.svc/docker.io/acme/tool:2"),
		Entry("the other names of Docker Hub", "registry-1.docker.io/acme/tool:2", "", mirrors, "cache.koinon.svc/docker.io/acme/tool:2"),
		Entry("a host with no entry and no registry stays", "quay.io/org/app:1", "", mirrors, "quay.io/org/app:1"),
		Entry("the registry replaces a host with no entry", "quay.io/org/app:1", "mirror.example.com/all", mirrors, "mirror.example.com/all/org/app:1"),
		Entry("a mirror wins over the registry", "ghcr.io/org/app:1", "mirror.example.com/all", mirrors, "cache.koinon.svc/ghcr.io/org/app:1"),
		Entry("a reference in the mirror is not prefixed twice", "cache.koinon.svc/ghcr.io/org/app:1", "", mirrors, "cache.koinon.svc/ghcr.io/org/app:1"),
		Entry("a mirror host with a port: the real host is the key, the path segment has no colon", "localhost:5001/koinon-e1/sandbox-substrate:e2e"+digest, "", map[string]string{"localhost:5001": "cache.koinon.svc/localhost_5001"},
			"cache.koinon.svc/localhost_5001/koinon-e1/sandbox-substrate:e2e"+digest),
		Entry("a host with a port", "localhost:5000/app:1", "r.example.com", nil, "r.example.com/app:1"),
		Entry("nothing is set", "ghcr.io/org/app:1", "", nil, "ghcr.io/org/app:1"),
	)
})

var _ = Describe("RequireDigest", func() {
	DescribeTable("accepts a reference pinned by digest",
		func(ref string) { Expect(substrate.RequireDigest(ref)).To(Succeed()) },
		Entry("digest only", "ghcr.io/org/app"+digest),
		Entry("tag and digest", "ghcr.io/org/app:1"+digest),
		Entry("a host with a port, a tag and a digest", "localhost:5000/app:1"+digest),
		Entry("a bare name with a digest", "busybox"+digest),
	)

	DescribeTable("refuses a reference that is not pinned by digest",
		func(ref string) {
			err := substrate.RequireDigest(ref)
			Expect(err).To(MatchError(ContainSubstring("pinned by digest")))
		},
		Entry("tag only", "ghcr.io/org/app:1"),
		Entry("a bare name", "busybox"),
		Entry("a host with a port and a tag", "localhost:5000/app:1"),
		Entry("empty", ""),
		Entry("a digest with no name", digest),
		Entry("a short digest", "ghcr.io/org/app:1@sha256:0123"),
		Entry("upper-case hex", "ghcr.io/org/app:1@sha256:"+strings.Repeat("A", 64)),
		Entry("another algorithm", "ghcr.io/org/app:1@sha512:"+strings.Repeat("a", 128)),
		Entry("text after the digest", "ghcr.io/org/app:1"+digest+"x"),
		Entry("two digests", "ghcr.io/org/app"+digest+digest),
	)
})

var _ = Describe("the actor template image", func() {
	DescribeTable("a template needs an image pinned by digest",
		func(image string, ok bool) {
			_, err := substrate.RenderTemplateForTest("ws", execution.SandboxSpec{Name: "a", Image: image}, substrate.Config{GuestSigner: fakeate.Signer()})
			if ok {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(err).To(MatchError(ContainSubstring("not pinned by digest")))
		},
		Entry("tag only is refused", "ghcr.io/org/app:1", false),
		Entry("digest only is accepted", "ghcr.io/org/app"+digest, true),
		Entry("tag and digest is accepted", "ghcr.io/org/app:1"+digest, true),
	)

	It("is refused as invalid at create", func(ctx SpecContext) {
		d, _ := newDriver()
		_, err := d.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: "tagged", Image: "ghcr.io/org/app:1"})
		Expect(err).To(MatchError(execution.ErrInvalid))
		Expect(err).To(MatchError(ContainSubstring("not pinned by digest")))
	})

	spec := execution.SandboxSpec{Name: "a", Image: "ghcr.io/org/app:1" + digest}

	It("goes through the image source, with the digest kept", func() {
		cfg := substrate.Config{GuestSigner: fakeate.Signer(), ImageMirrors: map[string]string{"ghcr.io": "cache.koinon.svc/ghcr.io"}}
		t, err := substrate.RenderTemplateForTest("ws", spec, cfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(t.GetContainers()[0].GetImage()).To(Equal("cache.koinon.svc/ghcr.io/org/app:1" + digest))
	})

	It("re-hashes the template when the mirror name changes", func() {
		a, err := substrate.RenderTemplateForTest("ws", spec, substrate.Config{GuestSigner: fakeate.Signer(), ImageMirrors: map[string]string{"ghcr.io": "a.svc/ghcr.io"}})
		Expect(err).NotTo(HaveOccurred())
		b, err := substrate.RenderTemplateForTest("ws", spec, substrate.Config{GuestSigner: fakeate.Signer(), ImageMirrors: map[string]string{"ghcr.io": "b.svc/ghcr.io"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.GetMetadata().GetName()).NotTo(Equal(b.GetMetadata().GetName()))
	})
})

var _ = Describe("the actor template command", func() {
	It("refuses a command: it would replace the guest front (issue #1092)", func(ctx SpecContext) {
		spec := execution.SandboxSpec{Name: "a", Image: "ghcr.io/org/app:1" + digest, Command: []string{"sleep", "infinity"}}
		_, err := substrate.RenderTemplateForTest("ws", spec, substrate.Config{GuestSigner: fakeate.Signer()})
		Expect(err).To(MatchError(ContainSubstring("replace the guest front")))

		d, _ := newDriver()
		_, err = d.CreateSandbox(ctx, ws, spec)
		Expect(err).To(MatchError(execution.ErrInvalid))
		Expect(err).To(MatchError(ContainSubstring("replace the guest front")))
	})

	It("leaves the entrypoint alone when no command is set", func() {
		t, err := substrate.RenderTemplateForTest("ws", execution.SandboxSpec{Name: "a", Image: "ghcr.io/org/app:1" + digest}, substrate.Config{GuestSigner: fakeate.Signer()})
		Expect(err).NotTo(HaveOccurred())
		Expect(t.GetContainers()[0].GetCommand()).To(BeEmpty())
	})
})
