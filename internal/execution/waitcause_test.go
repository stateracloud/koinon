// SPDX-License-Identifier: Apache-2.0

package execution_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
)

var _ = DescribeTable("ClassifySandboxWait",
	func(reason, message string, want execution.SandboxWaitCause) {
		Expect(execution.ClassifySandboxWait(reason, message)).To(Equal(want))
	},
	Entry("an image pull reason", "ErrImagePull", "", execution.WaitCauseImagePull),
	Entry("a back-off reason", "ImagePullBackOff", "", execution.WaitCauseImagePull),
	Entry("a pull error in the message", "", `failed to pull image "zot.example.test/x:1": pull access denied`, execution.WaitCauseImagePull),
	Entry("an admission webhook", "", `admission webhook "x" denied the request: no`, execution.WaitCauseAdmission),
	Entry("a ValidatingAdmissionPolicy", "", `pods "x" is forbidden: ValidatingAdmissionPolicy 'koinon-sandbox' with binding 'b' denied request: no`, execution.WaitCauseAdmission),
	Entry("a quota", "", "exceeded quota: compute", execution.WaitCauseAdmission),
	Entry("an unschedulable pod", "Unschedulable", "", execution.WaitCauseNoNodes),
	Entry("no node has the memory", "", "0/3 nodes are available: 3 Insufficient memory.", execution.WaitCauseNoNodes),
	Entry("nothing", "", "", execution.WaitCauseUnknown),
	Entry("a reason that says nothing", "Pending", "waiting", execution.WaitCauseUnknown),
	Entry("a reason in other case", "errimagepull", "", execution.WaitCauseImagePull),
)

var _ = DescribeTable("SandboxWaitCause.Describe",
	func(c execution.SandboxWaitCause, want string) { Expect(c.Describe()).To(Equal(want)) },
	Entry("image_pull", execution.WaitCauseImagePull, "the image could not be pulled"),
	Entry("admission", execution.WaitCauseAdmission, "the cluster's admission policy refused the sandbox"),
	Entry("no_nodes", execution.WaitCauseNoNodes, "no node can run the sandbox"),
	Entry("unknown", execution.WaitCauseUnknown, "the cause is unknown"),
)
