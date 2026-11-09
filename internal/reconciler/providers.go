// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"path"

	"github.com/jackc/pgx/v5/pgtype"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler/gatewayplan"
	"github.com/stateracloud/koinon/internal/thinking"
)

// Provider keys reach the gateway as files (plan item PV1, ADR 0056). The reconciler opens
// each served provider's key and writes it into one Secret, one data key per provider; the
// gateway mounts that Secret as a directory and the rendered config points every provider at
// its file (apiKey.file). No other component mounts it.
//
// A data key is "<provider ID>-<first 8 hex of the SHA-256 of the key's envelope>". A new key
// is a new file name, so the rendered config changes and agentgateway reloads it (a Secret
// volume updates in place without a pod restart, but agentgateway reads a key file only when
// it loads the config, and a config that did not change would never reload). The order of a
// pass is: write the new files, wait for kubelet to propagate them (Config.ProviderKeySettle;
// agentgateway rejects a config naming a file that is not there yet, and does not retry it),
// apply the config, then prune files no provider uses any more.

// ProviderKeyStore holds the gateway's provider key files.
type ProviderKeyStore interface {
	// Put adds or replaces files and keeps every other entry. It returns the names it wrote
	// that were not there with the same content, and every name the store holds afterwards.
	Put(ctx context.Context, files map[string][]byte) (written []string, present map[string]bool, err error)
	// Prune removes every entry not in keep.
	Prune(ctx context.Context, keep map[string]bool) error
}

// KubeProviderKeys keeps the files in one Secret of Koinon's namespace, created by the chart.
type KubeProviderKeys struct {
	Client    kubernetes.Interface
	Namespace string
	Name      string
}

var _ ProviderKeyStore = KubeProviderKeys{}

func (k KubeProviderKeys) update(ctx context.Context, edit func(data map[string][]byte) bool) error {
	secrets := k.Client.CoreV1().Secrets(k.Namespace)
	cur, err := secrets.Get(ctx, k.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		data := map[string][]byte{}
		edit(data)
		_, err = secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: k.Name, Namespace: k.Namespace, Labels: map[string]string{LabelManagedBy: ManagedByValue}},
			Type:       corev1.SecretTypeOpaque, Data: data,
		}, metav1.CreateOptions{FieldManager: ManagedByValue})
		if err != nil {
			return fmt.Errorf("create secret %s/%s: %w", k.Namespace, k.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get secret %s/%s: %w", k.Namespace, k.Name, err)
	}
	data := maps.Clone(cur.Data)
	if data == nil {
		data = map[string][]byte{}
	}
	if !edit(data) {
		return nil
	}
	cur.Data = data
	if _, err := secrets.Update(ctx, cur, metav1.UpdateOptions{FieldManager: ManagedByValue}); err != nil {
		return fmt.Errorf("update secret %s/%s: %w", k.Namespace, k.Name, err)
	}
	return nil
}

// Put implements ProviderKeyStore.
func (k KubeProviderKeys) Put(ctx context.Context, files map[string][]byte) ([]string, map[string]bool, error) {
	var written []string
	present := map[string]bool{}
	err := k.update(ctx, func(data map[string][]byte) bool {
		written = written[:0]
		for name, v := range files {
			if cur, ok := data[name]; !ok || string(cur) != string(v) {
				data[name] = v
				written = append(written, name)
			}
		}
		clear(present)
		for name := range data {
			present[name] = true
		}
		return len(written) > 0
	})
	return written, present, err
}

// Prune implements ProviderKeyStore.
func (k KubeProviderKeys) Prune(ctx context.Context, keep map[string]bool) error {
	return k.update(ctx, func(data map[string][]byte) bool {
		changed := false
		for name := range data {
			if !keep[name] {
				delete(data, name)
				changed = true
			}
		}
		return changed
	})
}

// servedProviders is what a pass renders for providers: the gateway providers and the key
// files they need.
type servedProviders struct {
	providers []gatewayplan.Provider
	files     map[string][]byte
	// modelIDs maps each rendered model (gateway.ModelRef) to its ID, for gateway_served_models.
	modelIDs map[string]pgtype.UUID
}

// loadProviders reads every org's enabled providers and models. A provider whose key cannot
// be opened is left out (its models are not served) and reported in the returned errors, so one
// broken entry does not stop the other orgs. A database error is returned as the second value
// and aborts the pass.
func (r *Reconciler) loadProviders(ctx context.Context, orgs []store.ListOrgsToReconcileRow) (servedProviders, []error, error) {
	out := servedProviders{files: map[string][]byte{}, modelIDs: map[string]pgtype.UUID{}}
	fileOf := map[string]string{}
	pidOf := map[string]pgtype.UUID{}
	var errs []error
	for _, org := range orgs {
		rows, err := r.q.ListServedCatalog(ctx, org.ID)
		if err != nil {
			return out, errs, fmt.Errorf("reconciler: list providers of org %s: %w", org.Slug, err)
		}
		byID := map[string]*gateway.Provider{}
		var order []string
		skip := map[string]bool{}
		for _, row := range rows {
			id := execution.UUIDString(row.ProviderID.Bytes)
			if skip[id] {
				continue
			}
			p, ok := byID[id]
			if !ok {
				if err := r.cfg.URLPolicy.CheckStrict(ctx, row.Url); err != nil {
					errs = append(errs, fmt.Errorf("provider %s/%s not served: %w", org.Slug, row.ProviderName, err))
					skip[id] = true
					continue
				}
				p = &gateway.Provider{Org: org.Slug, Name: row.ProviderName, Type: row.ProviderType, BaseURL: row.Url, AnthropicWorkspaceID: row.AnthropicWorkspaceID.String}
				if len(row.ApiKey) > 0 {
					key, err := catalog.OpenKey(r.cfg.Keys, org.ID, row.ProviderID, row.ApiKey)
					if err != nil {
						// Never include the error's text from the key itself: OpenKey's errors name no key.
						errs = append(errs, fmt.Errorf("provider %s/%s not served: its key cannot be opened: %w", org.Slug, row.ProviderName, err))
						skip[id] = true
						continue
					}
					sum := sha256.Sum256(row.ApiKey)
					name := id + "-" + hex.EncodeToString(sum[:4])
					out.files[name] = []byte(key)
					fileOf[id] = name
					p.APIKeyFile = path.Join(r.cfg.ProviderKeyDir, name)
				}
				byID[id] = p
				pidOf[id] = row.ProviderID
				order = append(order, id)
			}
			// The beta the gateway pins comes from the catalog's thinking capability, never from a client
			// (ADR 0069 amendment 2026-10-29).
			m := gateway.Model{Name: row.ModelName, Guard: row.Guard, Format: modelformat.Format(row.ModelFormat), Thinking: thinking.Of(row.ProviderType, row.ModelName, modelformat.Format(row.ModelFormat))}
			// The price valid now: the org's price entries, else the default list (plan item PV2).
			// A model with no price is not served.
			pm := prices.Model{ID: row.ModelID.String(), Name: row.ModelName, ProviderType: row.ProviderType, ProviderURL: row.Url, Format: modelformat.Format(row.ModelFormat)}
			var badPrice *prices.BadPriceError
			switch res, err := r.cfg.Prices.Resolve(ctx, r.q, org.ID, pm, r.cfg.Now()); {
			case err == nil:
				gp := res.Price.Gateway()
				m.Price = &gp
			case errors.As(err, &badPrice):
				// One org's unusable price skips that model, reported, and stops nothing else.
				errs = append(errs, fmt.Errorf("model %s/%s not served: %w", org.Slug, row.ModelName, err))
				continue
			case !errors.Is(err, prices.ErrUnpriced):
				return out, errs, fmt.Errorf("reconciler: price of model %s/%s: %w", org.Slug, row.ModelName, err)
			}
			p.Models = append(p.Models, m)
			out.modelIDs[gateway.ModelRef(org.Slug, row.ModelName)] = row.ModelID
		}
		for _, id := range order {
			out.providers = append(out.providers, gatewayplan.Provider{
				ID: id, OrgID: org.ID, ProviderID: pidOf[id], File: fileOf[id], Gateway: *byID[id],
			})
		}
	}
	if len(out.files) > 0 && r.cfg.ProviderKeys == nil {
		return out, errs, errors.New("reconciler: providers have keys but no provider key store is configured")
	}
	return out, errs, nil
}
