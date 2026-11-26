// SPDX-License-Identifier: Apache-2.0

package substrateauthz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// AtespacesPath is the one path of the provisioner's interface: POST {"name": "<workspace name>"}. The answer is
// 200 when the atespace exists and its policy is right, 400 for a name that is no workspace name, 401 for a
// missing or invalid token, 403 for a caller that is not allowed, and 502 when Substrate refused or was
// unreachable. The call is idempotent: the caller retries any non-200 answer on its next pass.
const AtespacesPath = "/v1/atespaces"

// Ensurer is what the server runs.
type Ensurer interface {
	EnsureAtespace(ctx context.Context, name string) error
}

// Authenticator turns a bearer token into the caller's username, or fails.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (string, error)
}

// Server is the provisioner's HTTP side. A token that is valid is not enough: the caller must be one of Callers,
// the usernames ("system:serviceaccount:<ns>:<name>") of the components that may ask. Today that is the reconciler.
type Server struct {
	Ensurer Ensurer
	Auth    Authenticator
	Callers []string
	Log     *slog.Logger
}

// Handler serves [AtespacesPath].
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+AtespacesPath, s.ensure)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Server) ensure(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		http.Error(w, "a bearer token is required", http.StatusUnauthorized)
		return
	}
	who, err := s.Auth.Authenticate(r.Context(), strings.TrimSpace(token))
	if err != nil {
		// The cause is logged, not answered: a TokenReview the API server forbids shows here.
		s.log().Warn("provisioner: token not accepted", "err", err)
		http.Error(w, "the token is not valid", http.StatusUnauthorized)
		return
	}
	if !slices.Contains(s.Callers, who) {
		s.log().Warn("provisioner: caller not allowed", "caller", who)
		http.Error(w, "this caller may not ask", http.StatusForbidden)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req); err != nil {
		http.Error(w, "the body is not the request", http.StatusBadRequest)
		return
	}
	if err := s.Ensurer.EnsureAtespace(r.Context(), req.Name); err != nil {
		if errors.Is(err, ErrName) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.log().Error("provisioner: ensure atespace", "atespace", req.Name, "err", err)
		http.Error(w, "Substrate refused or was unreachable", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// TokenReviewer authenticates a ServiceAccount token with the Kubernetes TokenReview API, for one audience.
// The provisioner's ServiceAccount needs the right to create a TokenReview (a ClusterRole in the chart).
type TokenReviewer struct {
	Kube     kubernetes.Interface
	Audience string
}

// Authenticate implements [Authenticator].
func (t TokenReviewer) Authenticate(ctx context.Context, token string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	got, err := t.Kube.AuthenticationV1().TokenReviews().Create(ctx, &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token, Audiences: []string{t.Audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("token review: %w", err)
	}
	if !got.Status.Authenticated {
		return "", errors.New("token review: not authenticated")
	}
	// A reviewer that ignores Spec.Audiences returns none: then the token is not for this audience.
	if !slices.Contains(got.Status.Audiences, t.Audience) {
		return "", errors.New("token review: the token is not for this audience")
	}
	return got.Status.User.Username, nil
}
