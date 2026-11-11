// SPDX-License-Identifier: Apache-2.0

package containerhost

// LocalStackImage is the LocalStack the awskms adapter suite and the chart smoke test run against:
// 4.14.0 is the last release that runs without a LocalStack account (later ones need an auth
// token). It is pinned by digest and kept here once so the two suites cannot drift apart.
// The short form (no docker.io/ prefix) is what hack/dep-age-check.sh can date today, see #537.
const LocalStackImage = "localstack/localstack:4.14.0@sha256:3ebc37595918b8accb852f8048fef2aff047d465167edd655528065b07bc364a"
