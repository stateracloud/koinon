# SPDX-License-Identifier: Apache-2.0
#
# Inner loop (ADR 0039, plan item X1): builds koinon-api, koinon-reconciler and koinon-agentd with ko,
# the flavor images with `make flavor-*`, and deploys charts/koinon (values: hack/dev/values*.yaml)
# into the namespace `koinon`. Two profiles, chosen by the kube context:
#
#   kind     context kind-koinon-*: images loaded into the node, runc, the fake model server and the
#            fake GitHub. Prerequisites: `make dev-kind-up`. Needs no configuration.
#   remote   any other context listed in the git-ignored hack/dev/local.yaml (template:
#            hack/dev/local.example.yaml): your registry, model provider, runtime class and OpenShell.
#            A maintainer's own lab is one example: keep its profile in the git-ignored local.yaml.
#
# `tilt up`, or for example `tilt up -- --flavors=opencode`. See docs/dev/loop.md.

config.define_string('mode', usage='kind or remote; default: from the kube context')
config.define_string('flavors', usage='comma-separated flavor images to build (default: opencode)')
config.define_string('platform', usage='linux/amd64 or linux/arm64; default: local.yaml platform (else amd64) on remote, the host on kind')
config.define_string('openshell-tls-secret', usage='Secret in the openshell namespace holding the client certificate (tls.crt, tls.key, ca.crt); copied into koinon. Empty: no mTLS (default: openshell.clientTLSSecret of local.yaml, else openshell-client-tls)')
config.define_string('openshell-namespace', usage='namespace of the OpenShell gateway (default: openshell.namespace of local.yaml, else openshell)')
config.define_string_list('helm-set', usage='extra chart values, key=value (helm --set)')
config.define_bool('sandbox-policy', usage='render the chart\'s cluster-wide sandbox ValidatingAdmissionPolicies on a remote cluster (default: sandboxPolicy of local.yaml, else off; always on for kind)')
config.define_bool('web', usage='run the Vite dev server for web/ (default: true)')
cfg = config.parse()

ctx = k8s_context()

# The remote profile reads hack/dev/local.yaml (git-ignored; see local.example.yaml for every key).
LOCAL_FILE = 'hack/dev/local.yaml'
local_cfg = {}
if os.path.exists(LOCAL_FILE):
    local_cfg = read_yaml(LOCAL_FILE) or {}
    watch_file(LOCAL_FILE)
remote_contexts = local_cfg.get('contexts', [])
if type(remote_contexts) != 'list':
    # `in` on a string is a substring match, so a bare name would also allow its prefixes.
    fail('`contexts` in the local profile must be a list of kube context names, e.g. contexts: [my-dev-cluster]')

mode = cfg.get('mode', '')
if not mode:
    if ctx.startswith('kind-koinon-'):
        mode = 'kind'
    elif ctx in remote_contexts:
        mode = 'remote'
if mode == 'remote' and ctx not in remote_contexts:
    fail('mode remote needs the kube context to be listed under `contexts` in %s (copy hack/dev/local.example.yaml), not %s' % (LOCAL_FILE, ctx))
if mode == 'kind' and not ctx.startswith('kind-koinon-'):
    fail('mode kind needs a kube context named kind-koinon-* (make dev-kind-up creates kind-koinon-dev), not %s' % ctx)
if mode not in ['remote', 'kind']:
    fail('kube context %s is not a Koinon dev cluster: use kind-koinon-* (make dev-kind-up), or list the context under `contexts` in %s (template: hack/dev/local.example.yaml)' % (ctx, LOCAL_FILE))

allow_k8s_contexts([ctx])

NS = 'koinon'
KIND = mode == 'kind'
os_cfg = local_cfg.get('openshell', {})
platform = cfg.get('platform', '') or ('linux/' + str(local('go env GOARCH', quiet=True)).strip() if KIND else local_cfg.get('platform', 'linux/amd64'))
registry = 'localhost:5001' if KIND else local_cfg.get('registry', '')
if not KIND and not os_cfg.get('address'):
    fail('%s needs `openshell.address` (the OpenShell gateway, e.g. openshell.openshell.svc:8080)' % LOCAL_FILE)
if not KIND and not registry:
    fail('%s needs `registry` (the registry and path Tilt pushes images to, e.g. registry.example.com/koinon)' % LOCAL_FILE)
os_ns = cfg.get('openshell-namespace', os_cfg.get('namespace', 'openshell'))
os_tls = cfg.get('openshell-tls-secret', os_cfg.get('clientTLSSecret', 'openshell-client-tls'))
sandbox_policy = KIND or cfg.get('sandbox-policy', local_cfg.get('sandboxPolicy', False))
flavors = [f for f in cfg.get('flavors', 'opencode').split(',') if f]
ALL_FLAVORS = ['opencode']
for f in flavors:
    if f not in ALL_FLAVORS:
        fail('unknown flavor %s (known: %s)' % (f, ', '.join(ALL_FLAVORS)))

# ---- images ---------------------------------------------------------------------------------

# ko builds a distroless image per binary. The unique tag is what Tilt injects into the pod specs;
# `dev` is the chart's image.tag, which the preflight Job's probe pods use. On kind ko pushes to the
# local registry that `make dev-kind-up` starts (localhost:5001, test/e2e/kind/registry.sh); on a remote
# cluster it pushes to your registry.
def ko_image(name):
    # The chart's images are <image.registry>/api, /reconciler and /agentd (#167); the binary is cmd/koinon-<name>.
    repo = registry + '/' + name
    ref_file = '.tilt-ko-' + name
    custom_build(
        repo,
        'TAG=tilt-$(date +%s) && ' +
        'KO_DOCKER_REPO="' + repo + '" ' +
        'ko build --bare --platform=' + platform + ' --tags "$TAG,dev" ./cmd/koinon-' + name +
        ' >/dev/null && echo "' + repo + ':$TAG" > "$TILT_KO_REF"',
        deps=['cmd', 'internal', 'go.mod', 'go.sum'],
        env={'TILT_KO_REF': ref_file},
        outputs_image_ref_to=ref_file,
        skips_local_docker=True,
    )

for n in ['api', 'reconciler', 'agentd']:
    ko_image(n)

# Flavor images are not in any pod spec Tilt sees (the reconciler starts sandboxes from them), so
# they are built by a local_resource; the chart gets the content-derived tag (hack/flavor-tag.sh),
# which changes the reconciler's config and rolls it when a flavor's files change.
watch_file('flavors')
flavor_tags = {}
for f in flavors:
    # The load-time tag is only the chart's value; the build computes its own, so a flavor edit
    # that has not reloaded the Tiltfile yet is still built and tagged consistently.
    flavor_tags[f] = str(local('hack/flavor-tag.sh ' + f, quiet=True)).strip()
    ref = '%s/flavors/%s:$T' % (registry, f)
    publish = 'docker push %s' % ref
    local_resource(
        'flavor-' + f,
        'T=$(hack/flavor-tag.sh %s) && make flavor-%s FLAVOR_TAG=$T FLAVOR_PLATFORMS=%s && docker tag koinon/flavor-%s:$T %s && %s' %
        (f, f, platform, f, ref, publish),
        deps=['flavors/' + f, 'flavors/common'],
        labels=['flavors'],
    )

if KIND:
    # The fakes' images are pushed to the local registry like the Koinon ones.
    custom_build(
        'localhost:5001/koinon-fakemodel',
        'TAG=tilt-$(date +%s) && KO_DOCKER_REPO="localhost:5001/koinon-fakemodel" ' +
        'ko build --bare --platform=' + platform + ' --tags "$TAG" ./cmd/koinon-fakemodel >/dev/null && ' +
        'echo "localhost:5001/koinon-fakemodel:$TAG" > "$TILT_KO_REF"',
        deps=['cmd/koinon-fakemodel', 'internal', 'test/fakemodel', 'go.mod', 'go.sum'],
        env={'TILT_KO_REF': '.tilt-ko-koinon-fakemodel'},
        outputs_image_ref_to='.tilt-ko-koinon-fakemodel',
        skips_local_docker=True,
    )
    custom_build(
        'localhost:5001/koinon-fakegithub',
        'TAG=tilt-$(date +%s) && KO_DOCKER_REPO="localhost:5001/koinon-fakegithub" ' +
        'ko build --bare --platform=' + platform + ' --tags "$TAG" ./cmd/koinon-fakegithub >/dev/null && ' +
        'echo "localhost:5001/koinon-fakegithub:$TAG" > "$TILT_KO_REF"',
        deps=['cmd/koinon-fakegithub', 'internal', 'go.mod', 'go.sum'],
        env={'TILT_KO_REF': '.tilt-ko-koinon-fakegithub'},
        outputs_image_ref_to='.tilt-ko-koinon-fakegithub',
        skips_local_docker=True,
    )
    custom_build(
        'localhost:5001/koinon-fakemcp',
        'TAG=tilt-$(date +%s) && KO_DOCKER_REPO="localhost:5001/koinon-fakemcp" ' +
        'ko build --bare --platform=' + platform + ' --tags "$TAG" ./cmd/koinon-fakemcp >/dev/null && ' +
        'echo "localhost:5001/koinon-fakemcp:$TAG" > "$TILT_KO_REF"',
        deps=['cmd/koinon-fakemcp', 'internal', 'go.mod', 'go.sum'],
        env={'TILT_KO_REF': '.tilt-ko-koinon-fakemcp'},
        outputs_image_ref_to='.tilt-ko-koinon-fakemcp',
        skips_local_docker=True,
    )

# ---- manifests ------------------------------------------------------------------------------

watch_file('hack/dev/namespace.yaml')
namespace_obj = decode_yaml(read_file('hack/dev/namespace.yaml'))
for k, v in local_cfg.get('namespaceLabels', {}).items():
    namespace_obj['metadata']['labels'][k] = v
k8s_yaml(encode_yaml(namespace_obj))
k8s_resource(new_name='namespace', objects=['koinon:namespace'], labels=['platform'])

if KIND:
    k8s_yaml('test/e2e/kind/fakes.yaml')
    k8s_resource(new_name='fakes-namespace', objects=['koinon-e2e-fakes:namespace'], labels=['fakes'])
    k8s_resource('fakemodel', resource_deps=['fakes-namespace'], labels=['fakes'])
    k8s_resource('fakegithub', resource_deps=['fakes-namespace'], labels=['fakes'])
    k8s_resource('fakemcp', resource_deps=['fakes-namespace'], labels=['fakes'])

# The chart is rendered with `helm template`, which is deterministic (the chart has no lookup and
# renders no password). Hooks are not run by `helm template | kubectl apply`, so the ensure-secrets
# hook Job, which creates the database and bootstrap-admin passwords, is applied as an ordinary
# resource below, before the database. The namespace is added to what the chart leaves without
# one, because Tilt applies the objects one by one.
values = ['hack/dev/values.yaml']
sets = ['image.tag=dev']
if KIND:
    values.append('hack/dev/values-kind.yaml')
    # Extra values files of a local.yaml apply on kind too (for example hack/dev/values-rt1.example.yaml
    # for the RT fixtures); the file's other keys are for remote clusters.
    values += local_cfg.get('values', [])
else:
    sets.append('image.registry=' + registry)
    if local_cfg.get('imagePullSecret'):
        sets.append('imagePullSecrets[0].name=' + local_cfg['imagePullSecret'])
    sets.append('openshell.address=' + os_cfg['address'])
    if os_cfg.get('runtimeClassName'):
        sets.append('openshell.runtimeClassName=' + os_cfg['runtimeClassName'])
    # Extra values files for what the keys above do not cover (network policy, egress hosts, ...).
    values += local_cfg.get('values', [])
if not sandbox_policy:
    # The policy's ValidatingAdmissionPolicy bindings are cluster-wide and would also deny writes in
    # the OpenShell sandbox namespaces the cluster already has.
    sets.append('sandboxPolicy.enabled=false')
    # The observer, NetworkPolicy writer and LimitRange writer all need the sandbox policy: the chart
    # refuses to render any of them without it (ADR 0078, spike 8.15, #76).
    sets.append('sandboxObserver.enabled=false')
    sets.append('sandboxNetworkPolicy.enabled=false')
    sets.append('sandboxLimitRange.enabled=false')
for f in ALL_FLAVORS:
    if f in flavors:
        sets.append('harnesses.%s.image.tag=%s' % (f, flavor_tags[f]))
    else:
        sets.append('harnesses.%s=null' % f)  # not built, so not offered to agents
if os_tls:
    sets.append('openshell.clientTLSSecret=' + os_tls)
else:
    sets.append('openshell.insecure=true')  # the chart needs one or the other
sets += cfg.get('helm-set', [])
watch_file('charts/koinon')
for v in values:
    watch_file(v)

cmd = ['helm', 'template', 'koinon', 'charts/koinon', '--namespace', NS]
for v in values:
    cmd += ['-f', v]
for s in sets:
    cmd += ['--set', s]
objs = decode_yaml_stream(local(cmd, quiet=True))
CLUSTER_SCOPED = ['ClusterRole', 'ClusterRoleBinding', 'ValidatingAdmissionPolicy', 'ValidatingAdmissionPolicyBinding']
NAMESPACED = ['ServiceAccount', 'Role', 'RoleBinding', 'PersistentVolumeClaim', 'ConfigMap', 'Secret',
              'NetworkPolicy', 'Service', 'Deployment', 'Job', 'Cluster']
for o in objs:
    if o['kind'] in CLUSTER_SCOPED:
        continue
    if o['kind'] not in NAMESPACED:
        fail('the chart renders a %s, which the Tiltfile does not know to be namespaced or cluster-scoped; add it to NAMESPACED or CLUSTER_SCOPED' % o['kind'])
    if not o['metadata'].get('namespace'):
        o['metadata']['namespace'] = NS
k8s_yaml(encode_yaml_stream(objs))
# Everything that is not a workload, the database or a Service (which Tilt attaches to its workload)
# is one resource that waits for the namespace, so that nothing is applied before it exists.
config_objs = []
for o in objs:
    if o['kind'] not in ['Deployment', 'Job', 'Service', 'Cluster']:
        config_objs.append('%s:%s' % (o['metadata']['name'], o['kind'].lower()))

cert_dep = []
if os_tls:
    # The OpenShell chart generates the client bundle in its own namespace; Koinon mounts a copy.
    local_resource(
        'openshell-client-cert',
        'kubectl --context %s -n %s get secret %s -o json | ' % (ctx, os_ns, os_tls) +
        "jq --arg ns %s '{apiVersion, kind, type, data, metadata: {name: .metadata.name, namespace: $ns}}' | " % NS +
        'kubectl --context %s apply -f -' % ctx,
        resource_deps=['namespace'],
        labels=['platform'],
    )
    cert_dep = ['openshell-client-cert']

# ---- resources ------------------------------------------------------------------------------

# The default org's model provider, declared with a labelled Secret that koinon-api imports (ADR 0056,
# plan item PV1): the fake model on kind; on a remote cluster the manifest named by `modelProvider` in
# local.yaml (none: add a provider in the UI).
provider_file = 'test/e2e/kind/model-provider.yaml' if KIND else local_cfg.get('modelProvider', '')
if provider_file:
    watch_file(provider_file)
    provider_objs = decode_yaml_stream(read_file(provider_file))
    k8s_yaml(encode_yaml_stream(provider_objs))
    k8s_resource(new_name='model-provider', objects=['%s:secret' % o['metadata']['name'] for o in provider_objs],
                 resource_deps=['namespace'], labels=['koinon'])

k8s_resource(new_name='koinon-config', objects=config_objs, resource_deps=['namespace'], labels=['koinon'])
# The generated passwords: created by `koinon-api ensure-secrets` (the chart's pre-install hook Job,
# applied here as an ordinary Job), never overwritten, so a re-run keeps the passwords of a running
# database. Everything that reads them waits for it.
k8s_resource('koinon-ensure-secrets', resource_deps=['koinon-config', 'koinon-preflight'], labels=['platform'])
k8s_resource(new_name='postgres', objects=['koinon-pg:cluster'], resource_deps=['koinon-config', 'koinon-ensure-secrets'], labels=['data'])
# The migration Job (on install, an ordinary release resource) waits for the database itself.
k8s_resource('koinon-migrate-install', resource_deps=['postgres'], labels=['data'])
k8s_resource('koinon-preflight', resource_deps=['koinon-config'], labels=['platform'])
k8s_resource('koinon-gateway', resource_deps=['koinon-config'], labels=['koinon'])
k8s_resource('koinon-api', port_forwards='8080:8080', resource_deps=['koinon-migrate-install'] + cert_dep, labels=['koinon'])
k8s_resource('koinon-reconciler', resource_deps=['koinon-migrate-install'] + cert_dep + ['flavor-' + f for f in flavors], labels=['koinon'])
k8s_resource('koinon-agentd', resource_deps=['koinon-migrate-install'] + cert_dep, labels=['koinon'])

if cfg.get('web', True):
    local_resource(
        'web-deps',
        'pnpm -C web install --frozen-lockfile',
        deps=['web/package.json', 'web/pnpm-lock.yaml'],
        labels=['web'],
    )
    local_resource(
        'web',
        serve_cmd='pnpm -C web dev',
        resource_deps=['web-deps'],
        labels=['web'],
    )
