// SPDX-License-Identifier: Apache-2.0

package savedwork

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/githubtools"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// Receiver takes saved work from koinon-agentd (upload.go): it checks the credential, the run and
// the bundle, seals the bundle with the key-encryption keys koinon-agentd does not have, stores
// it, and asks for the approval to push it. One upload is handled at a time, so a burst of stuck
// sandboxes cannot hold several 50 MB bundles in memory at once.
type Receiver struct {
	pool *pgxpool.Pool
	q    *store.Queries
	ring *secretstore.Keyring
	key  func(context.Context) ([]byte, error)
	log  *slog.Logger
	sem  chan struct{}
	now  func() time.Time
}

// NewReceiver returns a Receiver.
func NewReceiver(pool *pgxpool.Pool, ring *secretstore.Keyring, key func(context.Context) ([]byte, error), log *slog.Logger) *Receiver {
	if log == nil {
		log = slog.Default()
	}
	return &Receiver{pool: pool, q: store.New(pool), ring: ring, key: key, log: log, sem: make(chan struct{}, 1), now: time.Now}
}

func fail(w http.ResponseWriter, code int, msg string) { http.Error(w, msg, code) }

// ServeHTTP implements http.Handler for UploadPath.
func (rc *Receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	q := r.URL.Query()
	var org, runID pgtype.UUID
	epoch, e1 := strconv.ParseInt(q.Get("epoch"), 10, 64)
	exp, e2 := strconv.ParseInt(q.Get("exp"), 10, 64)
	n, e3 := strconv.Atoi(q.Get("n"))
	if org.Scan(q.Get("org")) != nil || runID.Scan(q.Get("run")) != nil || !org.Valid || !runID.Valid || e1 != nil || e2 != nil || e3 != nil || n < 1 || n > 20 {
		fail(w, http.StatusBadRequest, "bad parameters")
		return
	}
	key, err := rc.key(r.Context())
	if err != nil {
		rc.log.Error("saved work: reading the upload key failed", "err", err)
		fail(w, http.StatusInternalServerError, "internal error")
		return
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	nonce, bodySHA := r.Header.Get(NonceHeader), r.Header.Get(BodyHashHeader)
	if !hashPattern32.MatchString(nonce) || !hashPattern.MatchString(bodySHA) ||
		!ValidToken(key, token, org.String(), runID.String(), epoch, exp, n, bodySHA, nonce, rc.now()) {
		fail(w, http.StatusUnauthorized, "bad credential")
		return
	}
	if _, err := rc.q.GetRunForUpload(r.Context(), store.GetRunForUploadParams{OrgID: org, ID: runID, Epoch: epoch}); err != nil {
		fail(w, http.StatusUnprocessableEntity, "the run does not own its sandbox any more")
		return
	}
	// A credential is good once: a replay is refused before anything is read.
	if used, err := rc.q.InsertUploadNonce(r.Context(), store.InsertUploadNonceParams{OrgID: org, RunID: runID, Nonce: nonce}); err != nil {
		rc.log.Error("saved work: recording the nonce failed", "err", err)
		fail(w, http.StatusInternalServerError, "internal error")
		return
	} else if used == 0 {
		fail(w, http.StatusConflict, "this credential was used already")
		return
	}
	select {
	case rc.sem <- struct{}{}:
		defer func() { <-rc.sem }()
	case <-r.Context().Done():
		return
	}
	packed, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxUploadBytes))
	if err != nil {
		fail(w, http.StatusRequestEntityTooLarge, "the bundle is too large or unreadable")
		return
	}
	if BundleHash(packed) != bodySHA {
		fail(w, http.StatusUnauthorized, "the body is not the one the credential was made for")
		return
	}
	res, err := rc.store(r.Context(), org, runID, epoch, n, packed)
	var bad badRequest
	switch {
	case errors.As(err, &bad):
		fail(w, http.StatusUnprocessableEntity, bad.Error())
		return
	case err != nil:
		rc.log.Error("saved work: storing failed", "run", runID.String(), "err", err)
		fail(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

type badRequest string

func (b badRequest) Error() string { return string(b) }

// store validates, seals and records a bundle, and the approval to push it.
func (rc *Receiver) store(ctx context.Context, org, runID pgtype.UUID, epoch int64, n int, packed []byte) (UploadResult, error) {
	work, err := Unpack(packed) // checks the manifest: paths, modes, sizes
	if err != nil {
		return UploadResult{}, badRequest(err.Error())
	}
	run, err := rc.q.GetRunForUpload(ctx, store.GetRunForUploadParams{OrgID: org, ID: runID, Epoch: epoch})
	if err != nil {
		return UploadResult{}, badRequest("the run does not own its sandbox any more")
	}
	version, err := rc.q.GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: org, ID: run.AgentVersionID})
	if err != nil {
		return UploadResult{}, err
	}
	cfg, err := agentconfig.Parse(version.Config)
	if err != nil {
		return UploadResult{}, err
	}
	b, ok := cfg.Binding(connectors.GitHubID)
	bound := false
	if ok {
		for _, t := range b.Targets {
			bound = bound || strings.EqualFold(t, work.Manifest.Repo)
		}
	}
	if !bound {
		return UploadResult{}, badRequest("the run's agent version is not bound to " + work.Manifest.Repo)
	}
	id := pgtype.UUID{Bytes: uuid.Must(uuid.NewV7()), Valid: true}
	env, err := SealPacked(rc.ring, org.String(), id.String(), packed)
	if err != nil {
		return UploadResult{}, err
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return UploadResult{}, err
	}
	manifest, err := json.Marshal(work.Manifest)
	if err != nil {
		return UploadResult{}, err
	}
	branch := BranchName(run.TaskID.String(), rc.now())
	if n > 1 {
		branch += "-" + strconv.Itoa(n)
	}
	expires := pgtype.Timestamptz{Time: rc.now().Add(Retention), Valid: true}
	args := PushArgsJSON(id.String(), work.Manifest.Repo, work.Manifest.Base, branch, BundleHash(packed), ManifestHash(work.Manifest))
	var approval store.ApprovalRequest
	err = pgx.BeginFunc(ctx, rc.pool, func(tx pgx.Tx) error {
		q := rc.q.WithTx(tx)
		if _, err := q.InsertSavedWork(ctx, store.InsertSavedWorkParams{
			OrgID: org, ID: id, TaskID: run.TaskID, RunID: run.ID, AgentID: run.AgentID, Repo: work.Manifest.Repo,
			BaseSha: work.Manifest.Base, Branch: branch, Manifest: manifest, SizeBytes: work.Manifest.Bytes(), Envelope: envJSON, ExpiresAt: expires,
		}); err != nil {
			return err
		}
		approval, err = q.CreateSystemApprovalRequest(ctx, store.CreateSystemApprovalRequestParams{
			OrgID: org, TaskID: run.TaskID, RunID: run.ID, AgentID: run.AgentID, Connector: connectors.GitHubID, Tool: ToolPush,
			Target: connectors.GitHubID + ":" + work.Manifest.Repo, ArgsJson: args, ArgsSha256: BundleHash([]byte(args)), ExpiresAt: expires,
		})
		if err != nil {
			return err
		}
		payload, err := json.Marshal(map[string]any{
			"approval_id": approval.ID.String(), "tool": approval.Tool, "target": approval.Target, "args_sha256": approval.ArgsSha256,
			"decision": string(approval.Decision), "risk": json.RawMessage(`{}`), "system": true,
		})
		if err != nil {
			return err
		}
		_, err = q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
			OrgID: org, TaskID: run.TaskID, RunID: run.ID, Kind: githubtools.KindApprovalRequested,
			ActorKind: store.TaskActorKindSystem, Payload: payload,
		})
		return err
	})
	if err != nil {
		return UploadResult{}, err
	}
	rc.log.Info("saved work stored", "org", org.String(), "run", run.ID.String(), "saved_work", id.String(), "bytes", len(packed))
	return UploadResult{SavedWorkID: id.String(), ApprovalID: approval.ID.String(), Branch: branch}, nil
}
