# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the release writes the Substrate harness digest into the packaged chart (#1360).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (has, lacks, fail, ...) and
# the `sub` array of 44-execution-driver.sh.
#
# The release packages the chart with `go run ./hack/cmd/chartdigest` run on values.yaml first. This
# check does the same on a copy of the chart's values and renders the result with the Substrate
# driver: the default values (digest empty) leave the harness image without a digest, and the copy
# carries the one the tool wrote.
fake_digest=sha256:0000000000000000000000000000000000000000000000000000000000000001
# A subdirectory: chart-check.sh feeds "$out"/*.yaml to chartlint, vapcheck and kubeconform.
mkdir -p "$out/release-digest"
digest_values="$out/release-digest/values.yaml"
cp "$CHART/values.yaml" "$digest_values"
go run ./hack/cmd/chartdigest -values "$digest_values" -digest "$fake_digest" || fail "chartdigest must write the digest into the chart values"
lacks "the default chart values leave the harness image without a digest with substrate" 'image: .*/sandbox-substrate:[^@ ]+@sha256:' "${sub[@]}" --show-only templates/config.yaml
has "the chart values with the release digest render the substrate harness image by digest" "image: .*/sandbox-substrate:[^@ ]+@$fake_digest" "${sub[@]}" -f "$digest_values" --show-only templates/config.yaml
# The tool leaves everything else of the file alone.
[[ $(diff "$CHART/values.yaml" "$digest_values" | grep -c '^[<>]') == 2 ]] || fail "chartdigest must change exactly one line of the chart values"
# It refuses a digest that is not sha256:<64 hex>, and leaves the file as it was.
cp "$CHART/values.yaml" "$digest_values"
if go run ./hack/cmd/chartdigest -values "$digest_values" -digest sha256:abc 2>/dev/null; then fail "chartdigest must refuse a short digest"; fi
cmp -s "$CHART/values.yaml" "$digest_values" || fail "chartdigest must not change the file when it refuses"
