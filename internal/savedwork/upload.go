// SPDX-License-Identifier: Apache-2.0

package savedwork

// How saved work gets from koinon-agentd to Postgres (ADR 0054 amendment).
//
// koinon-agentd reads the files out of the sandbox but holds no key-encryption key: it posts the
// packed bundle to koinon-api (Receiver, on the tools listener only koinon-agentd and agentgateway
// may reach), which validates it, seals it and writes the rows. The post carries an HMAC over
// org, run, epoch and an expiry, made with a key both read from the database (service_keys), so
// a request is good for one run under one epoch for a few minutes.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// UploadPath is where the Receiver listens; KeyName names its key in service_keys.
const (
	UploadPath = "/internal/saved-work"
	KeyName    = "saved-work-upload"
	// MaxUploadBytes bounds a request body: the packed bundle of at most MaxBytes of files.
	MaxUploadBytes = MaxBytes + (8 << 20)
	// TokenLifetime is how long an upload token is good for.
	TokenLifetime = 5 * time.Minute
)

// Token is the bearer credential for an upload of the run (org, run) under epoch, good until exp.
func Token(key []byte, org, run string, epoch, exp int64) string {
	m := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(m, "saved-work\n%s\n%s\n%d\n%d", org, run, epoch, exp)
	return hex.EncodeToString(m.Sum(nil))
}

// ValidToken reports whether token matches and has not expired.
func ValidToken(key []byte, token, org, run string, epoch, exp int64, now time.Time) bool {
	if exp < now.Unix() || exp > now.Add(2*TokenLifetime).Unix() {
		return false
	}
	return hmac.Equal([]byte(token), []byte(Token(key, org, run, epoch, exp)))
}

// UploadResult is what the Receiver made of an upload.
type UploadResult struct {
	SavedWorkID string `json:"saved_work_id"`
	ApprovalID  string `json:"approval_id"`
	Branch      string `json:"branch"`
}

// Upload posts a packed bundle to the Receiver at baseURL for the run (org, run) under epoch.
// n numbers the repository within one incident, for the branch name.
func Upload(ctx context.Context, hc *http.Client, baseURL string, key []byte, org, run string, epoch int64, n int, packed []byte) (UploadResult, error) {
	exp := time.Now().Add(TokenLifetime).Unix()
	q := url.Values{
		"org": {org}, "run": {run}, "epoch": {strconv.FormatInt(epoch, 10)}, "exp": {strconv.FormatInt(exp, 10)}, "n": {strconv.Itoa(n)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(baseURL, "/")+UploadPath+"?"+q.Encode(), bytes.NewReader(packed))
	if err != nil {
		return UploadResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+Token(key, org, run, epoch, exp))
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := hc.Do(req)
	if err != nil {
		return UploadResult{}, err
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
	if res.StatusCode != http.StatusOK {
		return UploadResult{}, fmt.Errorf("koinon-api answered %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	var out UploadResult
	if err := json.Unmarshal(body, &out); err != nil || out.SavedWorkID == "" {
		return UploadResult{}, errors.New("koinon-api answered with something unreadable")
	}
	return out, nil
}
