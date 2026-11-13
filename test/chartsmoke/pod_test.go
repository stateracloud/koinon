// SPDX-License-Identifier: Apache-2.0

package chartsmoke_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

var metaGet = metav1.GetOptions{}

// runner starts the containers of rendered pods the way the kubelet would, as far as a plain
// Docker daemon allows. What it models, from the rendered pod spec: the image (mapped to the
// locally built one), command and args, env (secretKeyRef, configMapKeyRef, fieldRef and
// resourceFieldRef resolved against the API server and the pod), the volumes (Secrets and
// ConfigMaps as files with their defaultMode, owned by root and the pod's fsGroup like the
// kubelet makes them, in Docker volumes; emptyDir as tmpfs; the ServiceAccount token when it is
// mounted), the in-cluster environment, the user (runAsUser, runAsGroup, fsGroup and
// supplementalGroups), the memory limit and the container security context (read-only root
// file system, dropped capabilities, no privilege escalation). Anything else in the spec that
// the runner does not understand (envFrom, subPath, other volume sources, other fieldRefs)
// panics, so that a chart change cannot slip past unmodelled.
// What it cannot model: seccomp profiles beyond Docker's default, NetworkPolicies, Services and
// the restart semantics of probes.
type runner struct {
	cl     *cluster
	nwName string
	images map[string]string
	docker *testcontainers.DockerClient
	// helperImage is a local image with a shell, for populating volumes.
	helperImage string
	// started are the containers, and volumes the Docker volumes, to remove when the spec group ends.
	started       []testcontainers.Container
	dockerVolumes []string
}

// workload is a started container.
type workload struct {
	Name string
	Ctr  testcontainers.Container
	Spec corev1.Container
	Pod  corev1.PodSpec
}

// start creates the container of pod spec ps and starts it. With exitWait set it returns once the
// container has exited (a Job); otherwise as soon as it runs.
func (r *runner) start(ctx context.Context, owner string, ps corev1.PodSpec, c corev1.Container, exitWait time.Duration) *workload {
	ref := r.imageFor(c.Image)
	if len(c.EnvFrom) > 0 {
		panic(fmt.Sprintf("%s/%s: envFrom is not modelled; add it to runner.env", owner, c.Name))
	}
	// The pod cannot be created without its ServiceAccount (the kubelet would not start it).
	sa := ps.ServiceAccountName
	if sa == "" {
		sa = "default"
	}
	saObj, err := r.cl.Admin.CoreV1().ServiceAccounts(namespace).Get(ctx, sa, metaGet)
	if err != nil {
		panic(fmt.Sprintf("%s: ServiceAccount %q does not exist when the pod starts: %v", owner, sa, err))
	}
	vars, envList := r.env(ctx, owner, ps, c)
	args := make([]string, 0, len(c.Args))
	for _, a := range c.Args {
		args = append(args, expand(a, vars))
	}
	mounts, tmpfs := r.volumes(ctx, owner, ps, c)
	if role := irsaRole(saObj); role != "" {
		// What the EKS Pod Identity Webhook adds to a pod whose ServiceAccount names an IAM role
		// (IRSA), whatever automountServiceAccountToken says: the role, and a token of the
		// ServiceAccount the pod exchanges for credentials with STS. EKS projects it with the
		// audience sts.amazonaws.com; LocalStack does not check the token, so the suite's
		// ordinary token stands in.
		envList["AWS_ROLE_ARN"] = role
		envList["AWS_WEB_IDENTITY_TOKEN_FILE"] = irsaTokenDir + "/token"
		var fsGroup int
		if ps.SecurityContext != nil && ps.SecurityContext.FSGroup != nil {
			fsGroup = int(*ps.SecurityContext.FSGroup)
		}
		mounts = append(mounts, r.fileVolume(ctx, map[string]volumeFile{"token": {[]byte(r.cl.Token(ctx, sa)), 0o644}}, fsGroup, irsaTokenDir, true))
	}
	user, groups := runAs(ps, c)

	opts := []testcontainers.ContainerCustomizer{
		testcontainers.WithEnv(envList),
		testcontainers.WithCmd(args...),
		testcontainers.WithName(fmt.Sprintf("chartsmoke-%s-%s-%d", owner, c.Name, time.Now().UnixNano()%1_000_000)),
		testcontainers.WithConfigModifier(func(cfg *container.Config) {
			if user != "" {
				cfg.User = user
			}
		}),
		network.WithNetworkName([]string{owner}, r.nwName),
		testcontainers.WithMounts(mounts...),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.Tmpfs = tmpfs
			hc.GroupAdd = groups
			hc.ExtraHosts = append(hc.ExtraHosts, apiServerName+":host-gateway")
			if sc := c.SecurityContext; sc != nil {
				if sc.ReadOnlyRootFilesystem != nil && *sc.ReadOnlyRootFilesystem {
					hc.ReadonlyRootfs = true
				}
				if sc.AllowPrivilegeEscalation != nil && !*sc.AllowPrivilegeEscalation {
					hc.SecurityOpt = append(hc.SecurityOpt, "no-new-privileges")
				}
				if sc.Capabilities != nil {
					for _, cp := range sc.Capabilities.Drop {
						hc.CapDrop = append(hc.CapDrop, string(cp))
					}
				}
			}
			if m := c.Resources.Limits.Memory(); m != nil && !m.IsZero() {
				hc.Memory = m.Value()
			}
		}),
	}
	if len(c.Command) > 0 {
		opts = append(opts, testcontainers.WithEntrypoint(c.Command...))
	}
	var ports []string
	for _, p := range c.Ports {
		ports = append(ports, fmt.Sprintf("%d/tcp", p.ContainerPort))
	}
	if len(ports) > 0 {
		opts = append(opts, testcontainers.WithExposedPorts(ports...))
	}
	if exitWait > 0 {
		opts = append(opts, testcontainers.WithWaitStrategy(wait.ForExit().WithExitTimeout(exitWait)))
	}
	ctr, err := testcontainers.Run(ctx, ref, opts...)
	w := &workload{Name: owner + "/" + c.Name, Ctr: ctr, Spec: c, Pod: ps}
	if ctr != nil {
		r.started = append(r.started, ctr)
	}
	if err != nil {
		// A container that never started has no logs, and asking a nil one would hide the error.
		logs := ""
		if ctr != nil {
			logs = w.Logs(ctx)
		}
		panic(fmt.Sprintf("start %s: %v\n%s", w.Name, err, logs))
	}
	return w
}

// runAs is the container's user ("uid" or "uid:gid", empty for the image's) and supplementary
// groups: the pod's fsGroup and supplementalGroups, as the kubelet adds them.
func runAs(ps corev1.PodSpec, c corev1.Container) (string, []string) {
	var uid, gid *int64
	if psc := ps.SecurityContext; psc != nil {
		uid, gid = psc.RunAsUser, psc.RunAsGroup
	}
	if sc := c.SecurityContext; sc != nil {
		if sc.RunAsUser != nil {
			uid = sc.RunAsUser
		}
		if sc.RunAsGroup != nil {
			gid = sc.RunAsGroup
		}
	}
	user := ""
	if uid != nil {
		user = strconv.FormatInt(*uid, 10)
		if gid != nil {
			user += ":" + strconv.FormatInt(*gid, 10)
		}
	}
	var groups []string
	if psc := ps.SecurityContext; psc != nil {
		if psc.FSGroup != nil {
			groups = append(groups, strconv.FormatInt(*psc.FSGroup, 10))
		}
		for _, g := range psc.SupplementalGroups {
			groups = append(groups, strconv.FormatInt(g, 10))
		}
	}
	return user, groups
}

// imageFor maps a rendered Koinon image reference to the locally built image. Any other
// koinon-* image is a component this suite does not know yet.
func (r *runner) imageFor(ref string) string {
	name := imageName(ref)
	tag, ok := r.images[name]
	if !ok {
		panic(fmt.Sprintf("image %q is not one of %v: add the new image to koinonImages (or skip it as third-party)", ref, koinonImages))
	}
	return tag
}

// Logs returns the container's combined output.
func (w *workload) Logs(ctx context.Context) string {
	rc, err := w.Ctr.Logs(ctx)
	if err != nil {
		return "(no logs: " + err.Error() + ")"
	}
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(rc)
	return string(b)
}

// ExitCode is the container's exit code, or -1 while it runs.
func (w *workload) ExitCode(ctx context.Context) int {
	st, err := w.Ctr.State(ctx)
	if err != nil || st.Running {
		return -1
	}
	return st.ExitCode
}

// Running reports whether the container is still running.
func (w *workload) Running(ctx context.Context) bool {
	st, err := w.Ctr.State(ctx)
	return err == nil && st.Running
}

// portNumber resolves a probe's port, by number or by the container's port name.
func (w *workload) portNumber(p intOrName) int {
	if p.name == "" {
		return p.num
	}
	for _, cp := range w.Spec.Ports {
		if cp.Name == p.name {
			return int(cp.ContainerPort)
		}
	}
	panic(fmt.Sprintf("%s: probe port %q is not a named container port", w.Name, p.name))
}

type intOrName struct {
	num  int
	name string
}

// ProbeHTTP polls an httpGet probe (path, port by name or number) on the mapped port until it
// answers 2xx or 3xx, the way the kubelet judges it, or until the deadline. It gives up early
// when the container has exited. It returns "" on success, else why not.
func (w *workload) ProbeHTTP(ctx context.Context, pr *corev1.Probe, deadline time.Duration) string {
	h := pr.HTTPGet
	port := intOrName{num: h.Port.IntValue()}
	if h.Port.Type == intstr.String {
		port = intOrName{name: h.Port.StrVal}
	}
	n := w.portNumber(port)
	mapped, err := w.Ctr.MappedPort(ctx, fmt.Sprintf("%d/tcp", n))
	if err != nil {
		return fmt.Sprintf("port %d is not published: %v", n, err)
	}
	scheme := "http"
	if h.Scheme == corev1.URISchemeHTTPS {
		scheme = "https"
	}
	url := fmt.Sprintf("%s://127.0.0.1:%s%s", scheme, mapped.Port(), h.Path)
	// The kubelet does not verify a probe's certificate.
	httpClient := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	last := "no answer yet"
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		if !w.Running(ctx) {
			return fmt.Sprintf("the container exited with code %d before %s answered", w.ExitCode(ctx), url)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		for _, hd := range h.HTTPHeaders {
			if strings.EqualFold(hd.Name, "Host") {
				req.Host = hd.Value
			} else {
				req.Header.Set(hd.Name, hd.Value)
			}
		}
		resp, err := httpClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				return ""
			}
			last = fmt.Sprintf("%s answered %d", url, resp.StatusCode)
		} else {
			last = err.Error()
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Sprintf("%s did not become ready within %s: %s", url, deadline, last)
}

// env resolves the container's environment. It returns the variables by name (for $(NAME) in
// args) and the final list.
func (r *runner) env(ctx context.Context, owner string, ps corev1.PodSpec, c corev1.Container) (map[string]string, map[string]string) {
	vars := map[string]string{}
	set := func(k, v string) { vars[k] = v }
	for _, e := range c.Env {
		switch vf := e.ValueFrom; {
		case vf == nil:
			set(e.Name, expand(e.Value, vars))
		case vf.SecretKeyRef != nil:
			ref := vf.SecretKeyRef
			s := r.cl.Secret(ctx, ref.Name)
			if s == nil || s.Data[ref.Key] == nil {
				if ref.Optional != nil && *ref.Optional {
					continue
				}
				panic(fmt.Sprintf("%s: env %s: Secret %s has no key %q; the pod would not start (CreateContainerConfigError)", owner, e.Name, ref.Name, ref.Key))
			}
			set(e.Name, string(s.Data[ref.Key]))
		case vf.ConfigMapKeyRef != nil:
			ref := vf.ConfigMapKeyRef
			cm, err := r.cl.Admin.CoreV1().ConfigMaps(namespace).Get(ctx, ref.Name, metaGet)
			if err != nil || (cm.Data[ref.Key] == "" && cm.BinaryData[ref.Key] == nil) {
				if ref.Optional != nil && *ref.Optional {
					continue
				}
				panic(fmt.Sprintf("%s: env %s: ConfigMap %s has no key %q (%v)", owner, e.Name, ref.Name, ref.Key, err))
			}
			set(e.Name, cm.Data[ref.Key])
		case vf.FieldRef != nil:
			switch vf.FieldRef.FieldPath {
			case "metadata.namespace":
				set(e.Name, namespace)
			case "metadata.name":
				set(e.Name, owner+"-0")
			case "spec.serviceAccountName":
				set(e.Name, ps.ServiceAccountName)
			case "spec.nodeName":
				set(e.Name, "chartsmoke-node")
			case "status.podIP", "status.hostIP":
				set(e.Name, "127.0.0.1")
			default:
				panic(fmt.Sprintf("%s: env %s: fieldRef %q is not modelled; add it to runner.env", owner, e.Name, vf.FieldRef.FieldPath))
			}
		case vf.ResourceFieldRef != nil:
			set(e.Name, resourceField(vf.ResourceFieldRef, c))
		}
	}
	// What the kubelet adds to every container.
	set("KUBERNETES_SERVICE_HOST", apiServerName)
	set("KUBERNETES_SERVICE_PORT", r.cl.Port)
	set("KUBERNETES_SERVICE_PORT_HTTPS", r.cl.Port)
	return vars, vars
}

func resourceField(ref *corev1.ResourceFieldSelector, c corev1.Container) string {
	var q resource.Quantity
	switch ref.Resource {
	case "limits.memory":
		q = c.Resources.Limits[corev1.ResourceMemory]
	case "requests.memory":
		q = c.Resources.Requests[corev1.ResourceMemory]
	case "limits.cpu":
		q = c.Resources.Limits[corev1.ResourceCPU]
	case "requests.cpu":
		q = c.Resources.Requests[corev1.ResourceCPU]
	default:
		panic(fmt.Sprintf("resourceFieldRef %q is not modelled; add it to resourceField", ref.Resource))
	}
	div := ref.Divisor
	if div.IsZero() {
		div = resource.MustParse("1")
	}
	// Quantities round up to a whole number of divisors; limits unset read as the node's capacity
	// in a cluster, which the chart never relies on (it sets limits).
	if q.IsZero() {
		return "0"
	}
	return strconv.FormatInt((q.MilliValue()+div.MilliValue()-1)/div.MilliValue(), 10)
}

// expand replaces $(NAME) with the variable's value, as the kubelet does in args and env values;
// $$ is a literal $ and an unknown name is left as it is.
func expand(s string, vars map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '$':
			b.WriteByte('$')
			i++
		case '(':
			if end := strings.IndexByte(s[i+2:], ')'); end >= 0 {
				name := s[i+2 : i+2+end]
				if v, ok := vars[name]; ok {
					b.WriteString(v)
				} else {
					b.WriteString(s[i : i+3+end])
				}
				i += 2 + end
				continue
			}
			b.WriteByte('$')
		default:
			b.WriteByte('$')
		}
	}
	return b.String()
}

// volumes materialises the container's volumeMounts: Docker volumes filled with the files, and
// tmpfs mounts for emptyDir.
func (r *runner) volumes(ctx context.Context, owner string, ps corev1.PodSpec, c corev1.Container) (mounts testcontainers.ContainerMounts, tmpfs map[string]string) {
	tmpfs = map[string]string{}
	byName := map[string]corev1.Volume{}
	for _, v := range ps.Volumes {
		byName[v.Name] = v
	}
	var fsGroup int
	if ps.SecurityContext != nil && ps.SecurityContext.FSGroup != nil {
		fsGroup = int(*ps.SecurityContext.FSGroup)
	}
	for _, m := range c.VolumeMounts {
		v, ok := byName[m.Name]
		if !ok {
			panic(fmt.Sprintf("%s: volumeMount %q has no volume", owner, m.Name))
		}
		if m.SubPath != "" || m.SubPathExpr != "" {
			panic(fmt.Sprintf("%s: volumeMount %q uses subPath, which is not modelled", owner, m.Name))
		}
		var files map[string]volumeFile
		switch {
		case v.EmptyDir != nil:
			opt := "rw,mode=1777"
			if l := v.EmptyDir.SizeLimit; l != nil {
				opt += fmt.Sprintf(",size=%d", l.Value())
			}
			tmpfs[m.MountPath] = opt
			continue
		case v.Secret != nil:
			s := v.Secret
			sec := r.cl.Secret(ctx, s.SecretName)
			data := map[string][]byte{}
			if sec != nil {
				data = sec.Data
			} else if s.Optional == nil || !*s.Optional {
				panic(fmt.Sprintf("%s: Secret %s (volume %s) does not exist and is not optional; the pod would not start", owner, s.SecretName, v.Name))
			}
			files = volumeFiles(data, s.Items, s.DefaultMode)
		case v.ConfigMap != nil:
			cm, err := r.cl.Admin.CoreV1().ConfigMaps(namespace).Get(ctx, v.ConfigMap.Name, metaGet)
			data := map[string][]byte{}
			if err == nil {
				for k, val := range cm.Data {
					data[k] = []byte(val)
				}
				for k, val := range cm.BinaryData {
					data[k] = val
				}
			} else if v.ConfigMap.Optional == nil || !*v.ConfigMap.Optional {
				panic(fmt.Sprintf("%s: ConfigMap %s (volume %s): %v; the pod would not start", owner, v.ConfigMap.Name, v.Name, err))
			}
			files = volumeFiles(data, v.ConfigMap.Items, v.ConfigMap.DefaultMode)
		default:
			panic(fmt.Sprintf("%s: volume %s has a source this suite does not model; add it to runner.volumes", owner, v.Name))
		}
		mounts = append(mounts, r.fileVolume(ctx, files, fsGroup, m.MountPath, m.ReadOnly))
	}

	if ps.AutomountServiceAccountToken == nil || *ps.AutomountServiceAccountToken {
		sa := ps.ServiceAccountName
		if sa == "" {
			sa = "default"
		}
		files := map[string]volumeFile{
			"token":     {[]byte(r.cl.Token(ctx, sa)), 0o644},
			"ca.crt":    {r.cl.CACert, 0o644},
			"namespace": {[]byte(namespace), 0o644},
		}
		mounts = append(mounts, r.fileVolume(ctx, files, fsGroup, "/var/run/secrets/kubernetes.io/serviceaccount", true))
	}
	return mounts, tmpfs
}

// volumeFile is a file of a projected volume.
type volumeFile struct {
	data []byte
	mode int64
}

// volumeFiles are a Secret's or ConfigMap's keys as files, with defaultMode (0644 when unset).
// With items only the listed keys are projected, at their paths.
func volumeFiles(data map[string][]byte, items []corev1.KeyToPath, defaultMode *int32) map[string]volumeFile {
	mode := int64(0o644)
	if defaultMode != nil {
		mode = int64(*defaultMode)
	}
	files := map[string]volumeFile{}
	if len(items) == 0 {
		for k, b := range data {
			files[k] = volumeFile{b, mode}
		}
		return files
	}
	for _, it := range items {
		b, ok := data[it.Key]
		if !ok {
			panic(fmt.Sprintf("volume item %q is not in the source", it.Key))
		}
		m := mode
		if it.Mode != nil {
			m = int64(*it.Mode)
		}
		files[it.Path] = volumeFile{b, m}
	}
	return files
}

// fileVolume creates a Docker volume holding the files, owned by root and group fsGroup as the
// kubelet leaves a volume of a pod with an fsGroup (root and group 0 without one), and returns
// its mount. Docker writes into a volume through a container, so a helper container that is
// never started has the volume mounted while the files are copied in with their owners.
func (r *runner) fileVolume(ctx context.Context, files map[string]volumeFile, fsGroup int, target string, readOnly bool) testcontainers.ContainerMount {
	name := fmt.Sprintf("chartsmoke-%d", time.Now().UnixNano())
	r.dockerVolumes = append(r.dockerVolumes, name)
	helper, err := testcontainers.Run(ctx, r.helperImage,
		testcontainers.WithNoStart(),
		testcontainers.WithMounts(testcontainers.VolumeMount(name, "/vol")))
	if helper != nil {
		defer func() { _ = testcontainers.TerminateContainer(helper) }()
	}
	if err != nil {
		panic(fmt.Sprintf("create the helper container for volume %s: %v", name, err))
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	dirs := map[string]bool{}
	for _, p := range paths {
		for d := path.Dir(p); d != "." && !dirs[d]; d = path.Dir(d) {
			dirs[d] = true
			must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "vol/" + d + "/", Mode: 0o755, Gid: fsGroup}))
		}
		f := files[p]
		must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "vol/" + p, Mode: f.mode, Size: int64(len(f.data)), Gid: fsGroup}))
		_, err := tw.Write(f.data)
		must(err)
	}
	must(tw.Close())
	if _, err := r.docker.CopyToContainer(ctx, helper.GetContainerID(), client.CopyToContainerOptions{DestinationPath: "/", Content: &buf}); err != nil {
		panic(fmt.Sprintf("fill volume %s: %v", name, err))
	}
	return testcontainers.ContainerMount{Source: testcontainers.GenericVolumeMountSource{Name: name}, Target: testcontainers.ContainerMountTarget(target), ReadOnly: readOnly}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// Close removes every container and Docker volume the runner created.
func (r *runner) Close() {
	for _, c := range r.started {
		_ = testcontainers.TerminateContainer(c)
	}
	for _, v := range r.dockerVolumes {
		_, _ = r.docker.VolumeRemove(context.Background(), v, client.VolumeRemoveOptions{Force: true})
	}
}
