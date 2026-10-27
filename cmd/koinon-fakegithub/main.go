// SPDX-License-Identifier: Apache-2.0

// Command koinon-fakegithub serves the GitHub App fake from internal/connectors/github/githubtest
// as a binary, for the kind e2e test (plan item E1). It is a test fixture, never a product
// component: its App key is generated at start and handed out on /_fake/app.
//
// Besides the GitHub REST endpoints Koinon calls, it serves two test-only endpoints:
//
//	GET /_fake/app            {"appId", "installationId", "account", "privateKey"}
//	GET /_fake/repos/{repo}   the repository's state: refs, commits, pulls, write calls, and
//	                          the files of every branch
//	GET /_fake/ca             the PEM certificate to trust, with -tls
//
// With -api-prefix /api/v3 it serves the REST API under that path, as GitHub Enterprise Server
// does (the /_fake endpoints stay at the root). With -tls it serves HTTPS with a certificate it
// generates for -tls-hosts at start; -ca-file writes that certificate (self-signed, so also its
// own CA) for a connection's caBundle.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
	"github.com/stateracloud/koinon/internal/project"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	account := flag.String("account", "koinon-demo", "installation account (organisation login)")
	repos := flag.String("repos", "hello", "comma-separated repositories of the installation")
	appID := flag.Int64("app-id", 4242, "GitHub App ID")
	installationID := flag.Int64("installation-id", 777, "installation ID")
	apiPrefix := flag.String("api-prefix", "", "serve the REST API under this path, e.g. /api/v3 (GitHub Enterprise Server)")
	useTLS := flag.Bool("tls", false, "serve HTTPS with a generated self-signed certificate")
	tlsHosts := flag.String("tls-hosts", "localhost,127.0.0.1", "comma-separated host names and IPs of the generated certificate")
	caFile := flag.String("ca-file", "", "with -tls: write the generated certificate (the CA to trust) to this file")
	flag.Parse()
	prefix := strings.TrimRight(strings.TrimSpace(*apiPrefix), "/")
	if prefix != "" && !strings.HasPrefix(prefix, "/") {
		slog.Error("-api-prefix must start with /")
		os.Exit(2)
	}

	f := githubtest.NewUnstarted()
	f.Account, f.AppID, f.InstallationID = *account, *appID, *installationID
	f.Repositories = nil
	for r := range strings.SplitSeq(*repos, ",") {
		if r = strings.TrimSpace(r); r != "" {
			f.Repositories = append(f.Repositories, r)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /_fake/app", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"appId": f.AppID, "installationId": f.InstallationID, "account": f.Account, "privateKey": string(f.PEM),
		})
	})
	mux.HandleFunc("GET /_fake/repos/{repo}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("repo")
		snap := f.Snapshot(name)
		files := map[string]map[string]string{}
		for branch := range snap.Refs {
			files[branch] = map[string]string{}
			for p, b := range f.Files(name, branch) {
				files[branch][p] = string(b)
			}
		}
		writeJSON(w, map[string]any{
			"name": snap.Name, "defaultBranch": snap.DefaultBranch, "refs": snap.Refs, "commits": snap.Commits,
			"pulls": snap.Pulls, "comments": snap.Comments, "calls": snap.Calls, "files": files, "mints": f.Mints(),
		})
	})
	if prefix != "" {
		mux.Handle(prefix+"/", http.StripPrefix(prefix, f.Handler))
	} else {
		mux.Handle("/", f.Handler)
	}

	srv := &http.Server{Addr: *addr, Handler: logRequests(mux), ReadHeaderTimeout: 10 * time.Second}
	var caPEM []byte
	if *useTLS {
		cert, pemBytes, err := selfSigned(strings.Split(*tlsHosts, ","))
		if err != nil {
			slog.Error("generate the TLS certificate", "err", err)
			os.Exit(1)
		}
		caPEM = pemBytes
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		mux.HandleFunc("GET /_fake/ca", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/x-pem-file")
			_, _ = w.Write(caPEM)
		})
		if *caFile != "" {
			if err := os.WriteFile(*caFile, caPEM, 0o644); err != nil { //nolint:gosec // a public certificate
				slog.Error("write the CA file", "err", err)
				os.Exit(1)
			}
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("starting koinon-fakegithub", "version", project.Version, "addr", *addr, "apiPrefix", prefix, "tls", *useTLS,
		"account", f.Account, "repositories", f.Repositories)
	var serveErr error
	if *useTLS {
		serveErr = srv.ListenAndServeTLS("", "")
	} else {
		serveErr = srv.ListenAndServe()
	}
	if err := serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("koinon-fakegithub failed", "err", err)
		os.Exit(1)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// logRequests logs each request's method, path and status, for the e2e test's diagnostics.
func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// selfSigned makes a certificate for hosts (names and IPs) that is its own CA, and returns it
// with its PEM, which a connection's caBundle trusts.
func selfSigned(hosts []string) (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "koinon-fakegithub test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, h := range hosts {
		if h = strings.TrimSpace(h); h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}
