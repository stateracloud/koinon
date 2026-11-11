<!-- SPDX-License-Identifier: Apache-2.0 -->

# Web UI development

The web UI in `web/` is a React SPA (ADR 0022): Vite, TanStack Router and Query, shadcn/ui on
Tailwind v4, and a connect-web client generated from `api/proto` (`make generate`).

## Dev loop

Run the API and the Vite dev server side by side:

```sh
# terminal 1: the API on :8080 (needs Postgres; or use `tilt up`, docs/dev/loop.md)
go run ./cmd/koinon-api --migrate --database-url "$DATABASE_URL" \
  --bootstrap-password-file ./bootstrap-password --otlp-addr=

# (--otlp-addr= turns the internal OTLP receiver off; it never runs without its bearer token)

# terminal 2: the UI on :5173, proxying the API
cd web && pnpm dev
```

Vite proxies `/auth/*`, `/healthz` and the Connect RPC paths (`/koinon.*`) to `KOINON_API_URL`
(default `http://localhost:8080`). The proxy keeps the browser's origin, so the API's CSRF check
passes without `--trusted-origins`. The session cookie is `__Host-` and `Secure`; browsers accept
that on `http://localhost`.

## How koinon-api serves the UI

`web/webui.go` embeds `web/dist` when the API is built with the `webui` build tag:

```sh
make build                      # pnpm build, then go build -tags webui
# or by hand
pnpm -C web build && go build -tags webui ./cmd/koinon-api
```

Without the tag (`go build ./...`, `go test ./...`, CI's Go job) `web.Dist()` is nil and the API
serves no UI, so Go builds never depend on a Node toolchain. With it, every path that no API or
`/auth` handler claims gets the SPA: files under `dist/` as they are (fingerprinted `assets/` with a
one-year cache), `index.html` for client routes, with a strict `Content-Security-Policy`.

Container images built with `ko` (Tiltfile) do not set the tag yet; in the Tilt loop the UI runs on
the Vite dev server (`web` resource). The release image build must run `pnpm build` first and pass
`-tags webui`.

## Tests

- `pnpm test`: Vitest and Testing Library, against an in-memory Connect router transport. Files
  share one module registry (`isolate: false`) for speed. `pnpm test:isolated` runs each file in a
  fresh environment and the nightly `web-isolated` workflow runs it, so a spec must not rely on
  state another file leaves behind.
- `pnpm test:e2e`: one Playwright test (`pnpm exec playwright install chromium` once; CI runs it in
  the Playwright image, no install). It fakes the
  API inside the browser, including a dropped `WatchRun` stream; the real API path with the fake
  model is plan item E1.
