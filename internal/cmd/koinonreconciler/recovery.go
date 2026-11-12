// SPDX-License-Identifier: Apache-2.0

package koinonreconciler

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/installbinding"
	"github.com/stateracloud/koinon/internal/recovery"
	"github.com/stateracloud/koinon/internal/safego"
	"github.com/stateracloud/koinon/internal/tokens"
)

// startRecovery starts the handler of PlatformAdminRecovery requests (issue #512, ADR 0110 decision 6).
// It runs before the restore-hold wait, so a held install and an install whose API is down can still
// be recovered: it talks to Postgres and the Kubernetes API only. Without a policy name the handler is
// off (a chart without the recovery objects); a half-set pair is a configuration error.
func startRecovery(ctx context.Context, cfg config, pool *pgxpool.Pool, kube kubernetes.Interface, binding *installbinding.Binding, log *slog.Logger) error {
	if cfg.recoveryPolicy == "" && cfg.recoveryBinding == "" {
		return nil
	}
	if cfg.recoveryPolicy == "" || cfg.recoveryBinding == "" {
		return errors.New("--recovery-policy and --recovery-binding are set together")
	}
	rc, err := restConfig(cfg.kubeconfig)
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return err
	}
	svc := tokens.New(pool, nil)
	if binding != nil {
		svc = svc.WithBinding(binding)
	}
	c := &recovery.Controller{
		Dynamic: dyn, Kube: kube, Namespace: cfg.namespace, Policy: cfg.recoveryPolicy, Binding: cfg.recoveryBinding,
		Handler: &recovery.Handler{Pool: pool, Tokens: svc}, Log: log.With("component", "recovery"),
	}
	safego.Go(ctx, log, "platform-admin-recovery", func(ctx context.Context) { _ = c.Run(ctx) })
	return nil
}
