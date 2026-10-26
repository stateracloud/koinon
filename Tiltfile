# SPDX-License-Identifier: Apache-2.0
#
# Inner loop (ADR 0039, plan item X1): builds koinon-api, koinon-reconciler and koinon-agentd with ko,
# the flavor images with `make flavor-*`, and deploys charts/koinon (values: hack/dev/values*.yaml)
# into the namespace `koinon`. Two profiles, chosen by the kube context:
#
#   homelab  context homelab or homelab-koinon: Zot as registry, Kata, the existing OpenShell, Ollama.
#   kind     context kind-koinon-*: images loaded into the node, runc, the fake model server and the
#            fake GitHub. Prerequisites: `make dev-kind-up`.
#
# `tilt up`, or for example `tilt up -- --flavors=opencode`. See docs/dev/loop.md.

config.define_string('mode', usage='homelab or kind; default: from the kube context')
config.define_string('flavors', usage='comma-separated flavor images to build (default: opencode)')
config.define_string('platform', usage='linux/amd64 or linux/arm64; default: amd64 on homelab, the host on kind')
config.define_string('openshell-tls-secret', usage='Secret in the openshell namespace holding the client certificate (tls.crt, tls.key, ca.crt); copied into koinon. Empty: no mTLS')
config.define_string('openshell-namespace', usage='namespace of the OpenShell gateway (default: openshell)')
config.define_string_list('helm-set', usage='extra chart values, key=value (helm --set)')
config.define_bool('sandbox-policy', usage='render the chart\'s cluster-wide sandbox ValidatingAdmissionPolicies on the home lab (needs an approved Rivet request; always on for kind)')
config.define_bool('web', usage='run the Vite dev server for web/ (default: true)')
cfg = config.parse()

ctx = k8s_context()
HOMELAB_CONTEXTS = ['homelab', 'homelab-koinon']

mode = cfg.get('mode', '')
if not mode:
    if ctx in HOMELAB_CONTEXTS:
        mode = 'homelab'
    elif ctx.startswith('kind-koinon-'):
        mode = 'kind'
if mode == 'homelab' and ctx not in HOMELAB_CONTEXTS:
    fail('mode homelab needs the kube context homelab or homelab-koinon, not %s' % ctx)
if mode == 'kind' and not ctx.startswith('kind-koinon-'):
    fail('mode kind needs a kube context named kind-koinon-* (make dev-kind-up creates kind-koinon-dev), not %s' % ctx)
if mode not in ['homelab', 'kind']:
    fail('kube context %s is not a Koinon dev cluster: use homelab, homelab-koinon or kind-koinon-*' % ctx)

allow_k8s_contexts(HOMELAB_CONTEXTS + [ctx] if mode == 'kind' else HOMELAB_CONTEXTS)

NS = 'koinon'
KIND = mode == 'kind'
platform = cfg.get('platform', '') or ('linux/' + str(local('go env GOARCH', quiet=True)).strip() if KIND else 'linux/amd64')
registry = 'kind.local' if KIND else 'zot.apps.lab.example.invalid/koinon'
kind_cluster = ctx[len('kind-'):]
os_ns = cfg.get('openshell-namespace', 'openshell')
os_tls = cfg.get('openshell-tls-secret', 'openshell-client-tls')
flavors = [f for f in cfg.get('flavors', 'opencode').split(',') if f]
ALL_FLAVORS = ['opencode']
for f in flavors:
    if f not in ALL_FLAVORS:
        fail('unknown flavor %s (known: %s)' % (f, ', '.join(ALL_FLAVORS)))

# ---- images ---------------------------------------------------------------------------------

# ko builds a distroless image per binary. The unique tag is what Tilt injects into the pod specs;
# `dev` is the chart's image.tag, which the preflight Job's probe pods use. On kind ko loads the
# image into the node itself (KIND_CLUSTER_NAME); on the home lab it pushes to Zot.
def ko_image(name):
    repo = registry + '/' + name
    ref_file = '.tilt-ko-' + name
    custom_build(
        repo,
        'TAG=tilt-$(date +%s) && ' +
        'KO_DOCKER_REPO="' + repo + '" KIND_CLUSTER_NAME="' + kind_cluster + '" ' +
        'ko build --bare --platform=' + platform + ' --tags "$TAG,dev" ./cmd/' + name +
        ' >/dev/null && echo "' + repo + ':$TAG" > "$TILT_KO_REF"',
        deps=['cmd', 'internal', 'go.mod', 'go.sum'],
        env={'TILT_KO_REF': ref_file},
        outputs_image_ref_to=ref_file,
        skips_local_docker=True,
    )

for n in ['koinon-api', 'koinon-reconciler', 'koinon-agentd']:
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
    ref = '%s/flavor-%s:$T' % (registry, f)
    publish = ('kind load docker-image --name %s %s' % (kind_cluster, ref)) if KIND else ('docker push %s' % ref)
    local_resource(
        'flavor-' + f,
        'T=$(hack/flavor-tag.sh %s) && make flavor-%s FLAVOR_TAG=$T FLAVOR_PLATFORMS=%s && docker tag koinon/flavor-%s:$T %s && %s' %
        (f, f, platform, f, ref, publish),
        deps=['flavors/' + f, 'flavors/common'],
        labels=['flavors'],
    )

if KIND:
    # kind has no registry: the fakes' images are loaded like the Koinon ones.
    custom_build(
        'kind.local/koinon-fakemodel',
        'TAG=tilt-$(date +%s) && KO_DOCKER_REPO="kind.local/koinon-fakemodel" KIND_CLUSTER_NAME="' + kind_cluster + '" ' +
        'ko build --bare --platform=' + platform + ' --tags "$TAG" ./cmd/koinon-fakemodel >/dev/null && ' +
        'echo "kind.local/koinon-fakemodel:$TAG" > "$TILT_KO_REF"',
        deps=['cmd/koinon-fakemodel', 'internal', 'test/fakemodel', 'go.mod', 'go.sum'],
        env={'TILT_KO_REF': '.tilt-ko-koinon-fakemodel'},
        outputs_image_ref_to='.tilt-ko-koinon-fakemodel',
        skips_local_docker=True,
    )
    custom_build(
        'kind.local/koinon-fakegithub',
        'TAG=tilt-$(date +%s) && KO_DOCKER_REPO="kind.local/koinon-fakegithub" KIND_CLUSTER_NAME="' + kind_cluster + '" ' +
        'ko build --bare --platform=' + platform + ' --tags "$TAG" ./cmd/koinon-fakegithub >/dev/null && ' +
        'echo "kind.local/koinon-fakegithub:$TAG" > "$TILT_KO_REF"',
        deps=['cmd/koinon-fakegithub', 'internal', 'go.mod', 'go.sum'],
        env={'TILT_KO_REF': '.tilt-ko-koinon-fakegithub'},
        outputs_image_ref_to='.tilt-ko-koinon-fakegithub',
        skips_local_docker=True,
    )

# ---- manifests ------------------------------------------------------------------------------

k8s_yaml('hack/dev/namespace.yaml')
k8s_resource(new_name='namespace', objects=['koinon:namespace'], labels=['platform'])

if KIND:
    k8s_yaml('test/e2e/kind/fakes.yaml')
    k8s_resource(new_name='fakes-namespace', objects=['koinon-e2e-fakes:namespace'], labels=['fakes'])
    k8s_resource('fakemodel', resource_deps=['fakes-namespace'], labels=['fakes'])
    k8s_resource('fakegithub', resource_deps=['fakes-namespace'], labels=['fakes'])

# The chart is rendered with `helm template`, which is deterministic (the chart has no lookup and
# renders no password). Hooks are not run by `helm template | kubectl apply`, so the ensure-secrets
# hook Job, which creates the database and bootstrap-admin passwords, is applied as an ordinary
# resource below, before the database. The namespace is added to what the chart leaves without
# one, because Tilt applies the objects one by one.
values = ['hack/dev/values.yaml', 'hack/dev/values-%s.yaml' % mode]
sets = ['image.tag=dev']
if not KIND and not cfg.get('sandbox-policy', False):
    # The policy's ValidatingAdmissionPolicy bindings are cluster-wide and would also deny writes in
    # the platform's existing OpenShell sandbox namespaces.
    sets.append('sandboxPolicy.enabled=false')
for f in ALL_FLAVORS:
    if f in flavors:
        sets.append('flavors.%s.image.tag=%s' % (f, flavor_tags[f]))
    else:
        sets.append('flavors.%s=null' % f)  # not built, so not offered to agents
if os_tls:
    sets.append('openshell.clientTLSSecret=' + os_tls)
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
# plan item PV1): the fake model on kind, the lab's keyless Ollama on the home lab.
provider_file = 'test/e2e/kind/model-provider.yaml' if KIND else 'hack/dev/model-provider-homelab.yaml'
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
