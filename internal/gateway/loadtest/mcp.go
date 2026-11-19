// SPDX-License-Identifier: Apache-2.0

package loadtest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Endpoint is an MCP endpoint under load.
type Endpoint struct {
	// Driver names the gateway in the report ("koinon-gateway", "agentgateway", "direct").
	Driver string
	// URL is the full URL of the MCP endpoint.
	URL string
	// Headers are sent on every request: the agent's gateway key, or the server's credential for a
	// call that bypasses the gateway.
	Headers map[string]string
	// Tool is the tool a call names, as the gateway lists it (target_tool).
	Tool string
	// StreamTool is the long-stream tool, as the gateway lists it. Empty skips the stream scenario.
	StreamTool string
}

// transport is shared by every client of the suite. The default transport keeps 2 idle connections per
// host, so with many sessions nearly every call would open a new TCP connection and the run would
// measure the ephemeral port range (TIME_WAIT), not the gateway.
var transport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 0
	t.MaxIdleConnsPerHost = 4096
	return t
}()

type headerRT struct{ headers map[string]string }

func (h headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h.headers {
		r.Header.Set(k, v)
	}
	return transport.RoundTrip(r)
}

// connect opens one MCP session. onProgress, when set, counts progress notifications.
func (e Endpoint) connect(ctx context.Context, onProgress func()) (*mcp.ClientSession, error) {
	opts := &mcp.ClientOptions{}
	if onProgress != nil {
		opts.ProgressNotificationHandler = func(context.Context, *mcp.ProgressNotificationClientRequest) { onProgress() }
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "koinon-load", Version: "0"}, opts)
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             e.URL,
		HTTPClient:           &http.Client{Transport: headerRT{headers: e.Headers}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
}

// sessionWorker makes a worker that holds one session and runs call on it. After an error it drops
// the session and opens a new one on the next call, as a client that reconnects does; the open is
// part of that call's time and its error.
func (e Endpoint) sessionWorker(ctx context.Context, onProgress func(), call func(context.Context, *mcp.ClientSession) error) (Worker, error) {
	s, err := e.connect(ctx, onProgress)
	if err != nil {
		return Worker{}, err
	}
	closeSession := func() {
		if s != nil {
			_ = s.Close()
			s = nil
		}
	}
	return Worker{
		Close: closeSession,
		Op: func(ctx context.Context) error {
			if s == nil {
				var err error
				if s, err = e.connect(ctx, onProgress); err != nil {
					s = nil
					return err
				}
			}
			if err := call(ctx, s); err != nil {
				closeSession()
				return err
			}
			return nil
		},
	}, nil
}

func callOK(ctx context.Context, s *mcp.ClientSession, p *mcp.CallToolParams) error {
	res, err := s.CallTool(ctx, p)
	if err != nil {
		return err
	}
	if res.IsError {
		msg := ""
		for _, c := range res.Content {
			if t, ok := c.(*mcp.TextContent); ok {
				msg += t.Text
			}
		}
		return fmt.Errorf("tool result is an error: %s", strings.ReplaceAll(msg, "\n", " "))
	}
	return nil
}

// ToolCalls is the sustained-calls scenario: conc sessions each call the tool in a loop.
func (e Endpoint) ToolCalls(ctx context.Context, conc int, warmup, d time.Duration) (Result, error) {
	return e.run(ctx, Spec{
		Name: "tools/call", Concurrency: conc, Duration: d, Warmup: warmup,
		NewWorker: func(ctx context.Context, _ int) (Worker, error) {
			return e.sessionWorker(ctx, nil, func(ctx context.Context, s *mcp.ClientSession) error {
				return callOK(ctx, s, &mcp.CallToolParams{Name: e.Tool, Arguments: map[string]any{"title": "load"}})
			})
		},
	})
}

// Streams is the long-stream scenario: each call holds its response stream open for about
// steps*interval and receives steps progress notifications. The notes progressExpected (completed calls
// times steps) and progressReceived (all calls, also those that failed after some) show whether a
// gateway forwards them: received is at least expected when it does.
func (e Endpoint) Streams(ctx context.Context, conc, steps int, interval, d time.Duration) (Result, error) {
	var got, calls atomic.Int64
	res, err := e.run(ctx, Spec{
		Name: fmt.Sprintf("stream %dx%s", steps, interval), Concurrency: conc, Duration: d,
		NewWorker: func(ctx context.Context, _ int) (Worker, error) {
			return e.sessionWorker(ctx, func() { got.Add(1) }, func(ctx context.Context, s *mcp.ClientSession) error {
				p := &mcp.CallToolParams{Name: e.StreamTool, Arguments: map[string]any{"steps": steps, "interval_ms": interval.Milliseconds()}}
				p.SetProgressToken("load")
				if err := callOK(ctx, s, p); err != nil {
					return err
				}
				calls.Add(1)
				return nil
			})
		},
	})
	if res.Notes == nil {
		res.Notes = map[string]float64{}
	}
	res.Notes["progressReceived"] = float64(got.Load())
	res.Notes["progressExpected"] = float64(calls.Load() * int64(steps))
	return res, err
}

// Custom runs a spec of the driver's own (a drain or a restart) with this endpoint's name.
func (e Endpoint) Custom(ctx context.Context, spec Spec) (Result, error) { return e.run(ctx, spec) }

// NewSessionWorker is [Endpoint.sessionWorker] for a driver's own scenario.
func (e Endpoint) NewSessionWorker(ctx context.Context) (Worker, error) {
	return e.sessionWorker(ctx, nil, func(ctx context.Context, s *mcp.ClientSession) error {
		return callOK(ctx, s, &mcp.CallToolParams{Name: e.Tool, Arguments: map[string]any{"title": "load"}})
	})
}

func (e Endpoint) run(ctx context.Context, spec Spec) (Result, error) {
	res, err := Run(ctx, spec)
	res.Driver = e.Driver
	return res, err
}

// WriteResults appends results to dir/results.jsonl (one JSON object per line), so a run that
// stops half way keeps what it measured. dir is created when it is missing.
func WriteResults(dir string, results ...Result) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "results.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // a results file the person asked for
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, r := range results {
		if err := enc.Encode(r); err != nil {
			_ = f.Close()
			return err
		}
	}
	return f.Close()
}

// Settings of a run, from the environment (hack/load-gateway.sh sets them).
const (
	// EnvDuration is the length of each measured part ("15s").
	EnvDuration = "KOINON_LOAD_DURATION"
	// EnvConcurrency is the list of concurrency levels ("1,16,64,256").
	EnvConcurrency = "KOINON_LOAD_CONCURRENCY"
	// EnvOut is the directory that gets results.jsonl and report.md.
	EnvOut = "KOINON_LOAD_OUT"
)

// Duration reads EnvDuration, default 15 seconds.
func Duration() time.Duration {
	if d, err := time.ParseDuration(os.Getenv(EnvDuration)); err == nil && d > 0 {
		return d
	}
	return 15 * time.Second
}

// Levels reads EnvConcurrency, default 1, 16, 64 and 256.
func Levels() []int {
	var out []int
	for f := range strings.SplitSeq(os.Getenv(EnvConcurrency), ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return []int{1, 16, 64, 256}
	}
	return out
}

// OutDir reads EnvOut, default load-results under the working directory.
func OutDir() string {
	if d := os.Getenv(EnvOut); d != "" {
		return d
	}
	return "load-results"
}

// Save writes results to OutDir (results.jsonl, appended) and prints them as a Markdown table.
func Save(results []Result) error {
	if err := WriteResults(OutDir(), results...); err != nil {
		return err
	}
	return Report(os.Stdout, results)
}

// NewStreamWorker is a worker whose calls are streams of steps progress notifications, interval
// apart (the drain scenario wants calls that are in flight at the signal).
func (e Endpoint) NewStreamWorker(ctx context.Context, steps int, interval time.Duration) (Worker, error) {
	return e.sessionWorker(ctx, nil, func(ctx context.Context, s *mcp.ClientSession) error {
		p := &mcp.CallToolParams{Name: e.StreamTool, Arguments: map[string]any{"steps": steps, "interval_ms": interval.Milliseconds()}}
		p.SetProgressToken("load")
		return callOK(ctx, s, p)
	})
}
