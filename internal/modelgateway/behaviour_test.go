// SPDX-License-Identifier: Apache-2.0

package modelgateway_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/agentgateway"
	"github.com/stateracloud/koinon/internal/gateway/gatewaytest"
	"github.com/stateracloud/koinon/internal/gateway/usagewire"
	"github.com/stateracloud/koinon/internal/modelgateway"
)

func TestModelGateway(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "modelgateway")
}

// sink keeps the records the gateway hands out, as the usage source of the behaviour suite.
type sink struct {
	mu   sync.Mutex
	recs []gateway.UsageRecord
}

func (s *sink) put(r gateway.UsageRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The suite judges the record the ledger reads: it goes through the wire encoding and the
	// receiver's decoder, as it does between koinon-gateway and koinon-api.
	out, rejected, _ := agentgateway.Decode(usagewire.Encode([]gateway.UsageRecord{r}))
	if rejected != 0 || len(out) != 1 {
		panic("modelgateway test: the usage record does not survive the wire")
	}
	s.recs = append(s.recs, out[0])
}

func (s *sink) of(keyID string) []gateway.UsageRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []gateway.UsageRecord
	for _, r := range s.recs {
		if r.KeyID == keyID {
			out = append(out, r)
		}
	}
	return out
}

// modelDriver lets the behaviour suite drive the model path alone: it applies the model half of a State.
type modelDriver struct{ gw *modelgateway.Gateway }

func (d modelDriver) IssueKey(_ context.Context, a gateway.AgentRef) (gateway.IssuedKey, error) {
	return gateway.NewKey(a)
}
func (d modelDriver) Usage() gateway.UsageReader { return gateway.NoUsage{} }
func (d modelDriver) Apply(ctx context.Context, s gateway.State) (gateway.Revision, error) {
	return d.gw.ApplyModel(ctx, s.ModelConfig())
}

// The model cases of the gateway behaviour suite, run against the in-process model path as
// koinon-gateway serves it, and judged on the usage record it hands to its sink.
var _ = func() bool {
	dir, err := os.MkdirTemp("", "koinon-modelgateway-behaviour-")
	if err != nil {
		panic(err)
	}
	gatewaytest.RegisterModel(gatewaytest.Env{
		Name:            "koinon-gateway",
		Host:            "127.0.0.1",
		HeaderAllowlist: true,
		NoTranslation:   true,
		KeyFile:         func(name string) string { return filepath.Join(dir, name) },
		Start: func(_ context.Context, _ []int, files map[string]string, _ string) (*gatewaytest.Gateway, error) {
			for name, content := range files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
					return nil, err
				}
			}
			s := &sink{}
			gw := modelgateway.New(modelgateway.Options{Sink: s.put})
			mux := http.NewServeMux()
			for _, p := range gateway.RoutePaths() {
				mux.Handle(p, gw.Handler())
			}
			srv := httptest.NewServer(mux)
			return &gatewaytest.Gateway{
				Driver: modelDriver{gw: gw},
				URL:    srv.URL,
				Usage:  func(_ context.Context, keyID string) ([]gateway.UsageRecord, error) { return s.of(keyID), nil },
				Stop: func(context.Context) error {
					gw.Drain()
					srv.CloseClientConnections()
					srv.Close()
					return nil
				},
			}, nil
		},
	})
	return true
}()
