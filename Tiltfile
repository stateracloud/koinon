# SPDX-License-Identifier: Apache-2.0
#
# Inner loop against the home-lab Talos cluster (ADR 0039). Images are built
# with ko and pushed to the dev registry (docs/dev/registry.md).

allow_k8s_contexts('homelab')
default_registry('zot.apps.lab.example.invalid/koinon')

# With outputs_image_ref_to, Tilt only injects $EXPECTED_REGISTRY and needs a
# tagged reference back, so each build gets a unique tag.
def ko_image(name, pkg):
    repo = '$EXPECTED_REGISTRY/' + name
    custom_build(
        name,
        'TAG=tilt-$(date +%s) && KO_DOCKER_REPO="' + repo + '" ' +
        'ko build --bare --platform=linux/amd64 --tags "$TAG" ' + pkg +
        ' && echo "' + repo + ':$TAG" > "$TILT_KO_REF"',
        deps=['cmd', 'internal', 'go.mod', 'go.sum'],
        env={'TILT_KO_REF': '.tilt-ko-' + name},
        outputs_image_ref_to='.tilt-ko-' + name,
        skips_local_docker=True,
    )

ko_image('koinon-api', './cmd/koinon-api')
ko_image('koinon-reconciler', './cmd/koinon-reconciler')

k8s_yaml(kustomize('hack/dev'))

k8s_resource(new_name='postgres', objects=['koinon-pg:cluster'], labels=['data'])
k8s_resource('koinon-api', port_forwards='8080:8080', resource_deps=['postgres'], labels=['koinon'])
k8s_resource('koinon-reconciler', resource_deps=['postgres'], labels=['koinon'])

local_resource(
    'web',
    serve_cmd='pnpm -C web dev',
    deps=['web/package.json'],
    labels=['koinon'],
)
