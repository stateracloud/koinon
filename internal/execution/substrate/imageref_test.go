// SPDX-License-Identifier: Apache-2.0

package substrate_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
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
		Entry("user/name is on docker.io", "acme/tool:2", "", mirrors, "cache.koinon.svc/docker.io/acme/tool:2"),
		Entry("the other names of Docker Hub", "registry-1.docker.io/acme/tool:2", "", mirrors, "cache.koinon.svc/docker.io/acme/tool:2"),
		Entry("a host with no entry and no registry stays", "quay.io/org/app:1", "", mirrors, "quay.io/org/app:1"),
		Entry("the registry replaces a host with no entry", "quay.io/org/app:1", "mirror.example.com/all", mirrors, "mirror.example.com/all/org/app:1"),
		Entry("a mirror wins over the registry", "ghcr.io/org/app:1", "mirror.example.com/all", mirrors, "cache.koinon.svc/ghcr.io/org/app:1"),
		Entry("a reference in the mirror is not prefixed twice", "cache.koinon.svc/ghcr.io/org/app:1", "", mirrors, "cache.koinon.svc/ghcr.io/org/app:1"),
		Entry("a host with a port", "localhost:5000/app:1", "r.example.com", nil, "r.example.com/app:1"),
		Entry("nothing is set", "ghcr.io/org/app:1", "", nil, "ghcr.io/org/app:1"),
	)
})

var _ = Describe("the actor template image", func() {
	spec := execution.SandboxSpec{Name: "a", Image: "ghcr.io/org/app:1" + digest}

	It("goes through the image source, with the digest kept", func() {
		cfg := substrate.Config{ImageMirrors: map[string]string{"ghcr.io": "cache.koinon.svc/ghcr.io"}}
		t, err := substrate.RenderTemplateForTest("ws", spec, cfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(t.GetContainers()[0].GetImage()).To(Equal("cache.koinon.svc/ghcr.io/org/app:1" + digest))
	})

	It("re-hashes the template when the mirror name changes", func() {
		a, err := substrate.RenderTemplateForTest("ws", spec, substrate.Config{ImageMirrors: map[string]string{"ghcr.io": "a.svc/ghcr.io"}})
		Expect(err).NotTo(HaveOccurred())
		b, err := substrate.RenderTemplateForTest("ws", spec, substrate.Config{ImageMirrors: map[string]string{"ghcr.io": "b.svc/ghcr.io"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.GetMetadata().GetName()).NotTo(Equal(b.GetMetadata().GetName()))
	})
})
