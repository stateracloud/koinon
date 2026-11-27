// SPDX-License-Identifier: Apache-2.0

package preflight

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
)

func devOf(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Dev), nil //nolint:unconvert // Dev is int32 on some platforms
}

// SharedFilesystems is what the probe pod runs. Under the node root as mounted in the pod, it compares the device
// number of /var/lib/ate with the root and with /var/lib/kubelet. It returns what AteDir shares a filesystem with,
// and missing when AteDir does not exist. A node without /var/lib/kubelet is compared with the root only.
func SharedFilesystems() (shared []string, missing bool, err error) {
	ate, err := devOf(ateFSProbeRoot + AteDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("stat %s: %w", AteDir, err)
	}
	root, err := devOf(ateFSProbeRoot)
	if err != nil {
		return nil, false, fmt.Errorf("stat %s: %w", ateFSProbeRoot, err)
	}
	if ate == root {
		shared = append(shared, "root")
	}
	if kube, err := devOf(ateFSProbeRoot + "/var/lib/kubelet"); err == nil && kube == ate {
		shared = append(shared, "kubelet")
	}
	return shared, false, nil
}
