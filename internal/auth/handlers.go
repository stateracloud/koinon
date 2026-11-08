// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

// Mount registers the login endpoints on mux:
//
//	POST /auth/login          bootstrap admin password login
//	GET  /auth/oidc/start     begin OIDC login (optional ?return_to=/path)
//	GET  /auth/oidc/callback  OIDC redirect URI
//	POST /auth/logout         revoke the session and clear the cookie
//	GET  /auth/methods        which login methods are enabled, for the login page
//
// Wrap the whole mux with CSRFProtection.
func (s *Service) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /auth/login", s.handleLogin)
	mux.HandleFunc("GET /auth/oidc/start", s.handleOIDCStart)
	mux.HandleFunc("GET /auth/oidc/callback", s.handleOIDCCallback)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.HandleFunc("GET /auth/methods", s.handleMethods)
}

type methodsResponse struct {
	Password bool `json:"password"`
	OIDC     bool `json:"oidc"`
}

// handleMethods tells the login page which forms to show. It needs no
// session and reveals only configuration the login page shows anyway.
func (s *Service) handleMethods(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, methodsResponse{Password: s.cfg.BootstrapLogin, OIDC: s.oidc != nil})
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	PasswordChangeRequired bool `json:"passwordChangeRequired"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleLogin accepts {"username","password"} as JSON or as a form, and on
// success sets the session cookie and returns whether the password must be
// changed before anything else.
func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.BootstrapLogin {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req loginRequest
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch mt {
	case "application/json":
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{"malformed request"})
			return
		}
	case "application/x-www-form-urlencoded":
		if err := r.ParseForm(); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{"malformed request"})
			return
		}
		req.Username, req.Password = r.PostForm.Get("username"), r.PostForm.Get("password")
	default:
		writeJSON(w, http.StatusUnsupportedMediaType, errorResponse{"use application/json"})
		return
	}

	cred, ok, err := s.checkPassword(r.Context(), req.Username, req.Password)
	if errors.Is(err, errBusy) {
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusTooManyRequests, errorResponse{"too many login attempts; retry shortly"})
		return
	}
	if err != nil {
		s.log.Error("password login failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{"internal error"})
		return
	}
	if !ok {
		s.log.Info("password login rejected")
		writeJSON(w, http.StatusUnauthorized, errorResponse{"invalid username or password"})
		return
	}

	if old, err := r.Cookie(s.SessionCookie()); err == nil {
		_ = s.revokeToken(r.Context(), old.Value)
	}
	var token string
	_, err = audit.InTx(r.Context(), s.pool, func(q *store.Queries, b audit.Batch) error {
		var err error
		token, _, err = s.createSession(r.Context(), q, b, cred.UserID, store.AuthMethodPassword)
		return err
	})
	if err != nil {
		s.log.Error("creating session failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{"internal error"})
		return
	}
	s.setSessionCookie(w, token)
	s.log.Info("user logged in", "method", "password")
	writeJSON(w, http.StatusOK, loginResponse{PasswordChangeRequired: cred.MustChangePassword})
}

var errBusy = errors.New("too many concurrent password checks")

// checkPassword verifies a local password. Unknown usernames cost the same
// as wrong passwords.
func (s *Service) checkPassword(ctx context.Context, username, password string) (store.LocalCredential, bool, error) {
	select {
	case s.verifySlots <- struct{}{}:
		defer func() { <-s.verifySlots }()
	default:
		return store.LocalCredential{}, false, errBusy
	}
	cred, err := s.q.GetLocalCredentialByUsername(ctx, username)
	if errors.Is(err, pgx.ErrNoRows) {
		_, _ = VerifyPasswordContext(ctx, dummyHash(), password)
		return store.LocalCredential{}, false, nil
	}
	if err != nil {
		return store.LocalCredential{}, false, err
	}
	ok, err := VerifyPasswordContext(ctx, cred.PasswordHash, password)
	if err != nil || !ok {
		return store.LocalCredential{}, false, err
	}
	if _, err := s.q.GetActiveUser(ctx, cred.UserID); errors.Is(err, pgx.ErrNoRows) {
		return store.LocalCredential{}, false, nil
	} else if err != nil {
		return store.LocalCredential{}, false, err
	}
	return cred, true, nil
}

// handleLogout revokes the current session. It always clears the cookie.
func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.SessionCookie()); err == nil {
		if err := s.logout(r.Context(), c.Value); err != nil {
			s.log.Error("revoking session failed", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	s.clearCookie(w, s.SessionCookie(), "/")
	w.WriteHeader(http.StatusNoContent)
}
