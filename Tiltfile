# SPDX-License-Identifier: Apache-2.0
#
# Inner loop against the home-lab Talos cluster (ADR 0039). Images are built
# with ko and pushed to the dev registry (docs/dev/registry.md).

allow_k8s_contexts('homelab')
default_registry('registry.apps.lab.example.invalid/koinon')

def ko_image(name, pkg):
    custom_build(
        name,
        'KO_DOCKER_REPO="$EXPECTED_REGISTRY/$EXPECTED_IMAGE" ' +
        'ko build --bare --platform=linux/amd64 --image-refs "$TILT_KO_REF" ' + pkg +
        ' && cat "$TILT_KO_REF"',
        deps=['cmd', 'internal', 'go.mod', 'go.sum'],
        env={'TILT_KO_REF': '.tilt-ko-' + name},
        outputs_image_ref_to='.tilt-ko-' + name,
        skips_local_docker=True,
    )

ko_image('koinon-api', './cmd/koinon-api')
ko_image('koinon-reconciler', './cmd/koinon-reconciler')

k8s_yaml(kustomize('hack/dev'))

k8s_resource('postgres', labels=['data'])
k8s_resource('koinon-api', port_forwards='8080:8080', resource_deps=['postgres'], labels=['koinon'])
k8s_resource('koinon-reconciler', resource_deps=['postgres'], labels=['koinon'])

local_resource(
    'web',
    serve_cmd='pnpm -C web dev',
    deps=['web/package.json'],
    labels=['koinon'],
)
