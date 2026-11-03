// SPDX-License-Identifier: Apache-2.0

package awskms

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// The pod's identity, as EKS gives it to a pod: never an access key in configuration.
//
//   - IRSA (IAM roles for service accounts): the EKS Pod Identity webhook sees the annotation
//     eks.amazonaws.com/role-arn on the pod's ServiceAccount and sets AWS_ROLE_ARN and
//     AWS_WEB_IDENTITY_TOKEN_FILE, a projected service account token for the audience
//     sts.amazonaws.com. The adapter trades it for temporary credentials with STS
//     AssumeRoleWithWebIdentity, which is an unsigned call: the token is the proof.
//   - EKS Pod Identity: the EKS Pod Identity Agent on the node serves credentials for the pod's
//     ServiceAccount at AWS_CONTAINER_CREDENTIALS_FULL_URI (http://169.254.170.23/v1/credentials),
//     and the pod proves who it is with the token in AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE.
//
// Both token files are read again on every refresh: the kubelet rotates them.
const (
	EnvRoleARN              = "AWS_ROLE_ARN"
	EnvWebIdentityTokenFile = "AWS_WEB_IDENTITY_TOKEN_FILE" //nolint:gosec // an env var name, not a credential
	EnvRoleSessionName      = "AWS_ROLE_SESSION_NAME"
	EnvCredentialsURI       = "AWS_CONTAINER_CREDENTIALS_FULL_URI"     //nolint:gosec // an env var name, not a credential
	EnvCredentialsTokenFile = "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE" //nolint:gosec // an env var name, not a credential
)

// staticKeyEnv are the AWS SDK's static credentials. The adapter never reads them.
var staticKeyEnv = []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"}

// Identity is how the pod obtains credentials. It holds file paths and a role ARN, never a credential.
type Identity struct {
	// RoleARN and WebIdentityTokenFile are IRSA's (AWS_ROLE_ARN, AWS_WEB_IDENTITY_TOKEN_FILE).
	RoleARN              string
	WebIdentityTokenFile string
	// RoleSessionName names the STS session in CloudTrail; default "koinon".
	RoleSessionName string
	// CredentialsURI and CredentialsTokenFile are EKS Pod Identity's
	// (AWS_CONTAINER_CREDENTIALS_FULL_URI, AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE).
	CredentialsURI       string
	CredentialsTokenFile string
}

// IdentityFromEnv reads the pod's identity from the variables IRSA's webhook or the EKS Pod
// Identity Agent set. ignored names the static key variables that are set: the adapter does not use
// them (they would be a credential in the pod's configuration), and the caller says so.
func IdentityFromEnv() (id Identity, ignored []string) {
	id = Identity{
		RoleARN:              os.Getenv(EnvRoleARN),
		WebIdentityTokenFile: os.Getenv(EnvWebIdentityTokenFile),
		RoleSessionName:      os.Getenv(EnvRoleSessionName),
		CredentialsURI:       os.Getenv(EnvCredentialsURI),
		CredentialsTokenFile: os.Getenv(EnvCredentialsTokenFile),
	}
	for _, k := range staticKeyEnv {
		if os.Getenv(k) != "" {
			ignored = append(ignored, k)
		}
	}
	return id, ignored
}

// credentials are temporary AWS credentials. They never appear in an error or a log.
type credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// Expires is when they stop working; zero is unknown.
	Expires time.Time
}

func (c credentials) valid() bool { return c.AccessKeyID != "" && c.SecretAccessKey != "" }

// String keeps credentials out of a %v.
func (c credentials) String() string { return "awskms.credentials{redacted}" }

// GoString keeps credentials out of a %#v.
func (c credentials) GoString() string { return c.String() }

// errNoIdentity means neither IRSA nor EKS Pod Identity is set up for the pod's ServiceAccount.
var errNoIdentity = errors.New("no pod identity: annotate the ServiceAccount with eks.amazonaws.com/role-arn (IRSA) " +
	"or create an EKS Pod Identity association for it; static access keys are not used")

// validCredentialsURI is the AWS SDKs' rule for a container credentials endpoint: https, or plain
// http only to the loopback or to the ECS and EKS agents' link-local addresses.
//
// https to any host is accepted, as the SDKs accept it: the URI comes from the pod's env, which the
// EKS Pod Identity webhook sets (to http://169.254.170.23 in practice), and whoever controls that
// env also controls the token file and every other input; https at least keeps the token off the
// wire in the clear. The adapter does not narrow it further, so a pod set up for the SDKs works
// unchanged. The answer is still bounded (maxResponse), never followed on a redirect, and the
// agent client never uses a proxy.
func validCredentialsURI(raw string, anyHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s is not a URL", EnvCredentialsURI)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if anyHTTP || loopback(h) || h == "169.254.170.2" || h == "169.254.170.23" || h == "fd00:ec2::23" {
			return nil
		}
	}
	return fmt.Errorf("%s must be https, or http to the loopback or the EKS Pod Identity Agent", EnvCredentialsURI)
}

// webIdentity trades the IRSA token for credentials with STS AssumeRoleWithWebIdentity.
func (w *Wrapper) webIdentity(ctx context.Context) (credentials, error) {
	jwt, err := os.ReadFile(w.cfg.Identity.WebIdentityTokenFile)
	if err != nil {
		return credentials{}, fmt.Errorf("read the web identity token: %w", err)
	}
	session := w.cfg.Identity.RoleSessionName
	if session == "" {
		session = "koinon"
	}
	form := url.Values{
		"Action":           {"AssumeRoleWithWebIdentity"},
		"Version":          {"2011-06-15"},
		"RoleArn":          {w.cfg.Identity.RoleARN},
		"RoleSessionName":  {session},
		"WebIdentityToken": {strings.TrimSpace(string(jwt))},
	}
	clear(jwt)
	body := []byte(form.Encode())
	defer clear(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.sts.String(), strings.NewReader(string(body)))
	if err != nil {
		return credentials{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	status, raw, err := w.do(w.http, req)
	if err != nil {
		return credentials{}, fmt.Errorf("sts: %w", err)
	}
	if status >= 300 && status < 400 {
		return credentials{}, fmt.Errorf("sts: HTTP %d: %w", status, errRedirect)
	}
	if status != http.StatusOK {
		var e struct {
			Error struct {
				Code    string `xml:"Code"`
				Message string `xml:"Message"`
			} `xml:"Error"`
		}
		if xml.Unmarshal(raw, &e) != nil || e.Error.Code == "" {
			return credentials{}, fmt.Errorf("sts: assume role %s refused: HTTP %d", w.cfg.Identity.RoleARN, status)
		}
		return credentials{}, fmt.Errorf("sts: assume role %s refused: HTTP %d: %s: %s", w.cfg.Identity.RoleARN, status,
			e.Error.Code, short(e.Error.Message))
	}
	var r struct {
		Result struct {
			Credentials struct {
				AccessKeyID     string `xml:"AccessKeyId"`
				SecretAccessKey string `xml:"SecretAccessKey"`
				SessionToken    string `xml:"SessionToken"`
				Expiration      string `xml:"Expiration"`
			} `xml:"Credentials"`
		} `xml:"AssumeRoleWithWebIdentityResult"`
	}
	if err := xml.Unmarshal(raw, &r); err != nil {
		return credentials{}, errors.New("sts: an answer that is not STS's")
	}
	rc := r.Result.Credentials
	c := credentials{AccessKeyID: rc.AccessKeyID, SecretAccessKey: rc.SecretAccessKey, SessionToken: rc.SessionToken}
	if t, err := time.Parse(time.RFC3339, rc.Expiration); err == nil {
		c.Expires = t
	}
	if !c.valid() {
		return credentials{}, errors.New("sts: an answer without credentials")
	}
	return c, nil
}

// podIdentity asks the EKS Pod Identity Agent for credentials.
func (w *Wrapper) podIdentity(ctx context.Context) (credentials, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.cfg.Identity.CredentialsURI, nil)
	if err != nil {
		return credentials{}, errors.New("pod identity: invalid credentials URI")
	}
	if f := w.cfg.Identity.CredentialsTokenFile; f != "" {
		tok, err := os.ReadFile(f) //nolint:gosec // the token file EKS Pod Identity names in the env
		if err != nil {
			return credentials{}, fmt.Errorf("read the pod identity token: %w", err)
		}
		req.Header.Set("Authorization", strings.TrimSpace(string(tok)))
		clear(tok)
	}
	status, raw, err := w.do(w.agent, req)
	if err != nil {
		return credentials{}, fmt.Errorf("pod identity: %w", err)
	}
	if status >= 300 && status < 400 {
		return credentials{}, fmt.Errorf("pod identity: HTTP %d: %w", status, errRedirect)
	}
	var r struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string `json:"SecretAccessKey"`
		Token           string `json:"Token"`
		Expiration      string `json:"Expiration"`
		Code            string `json:"code"`
		Message         string `json:"message"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return credentials{}, fmt.Errorf("pod identity: HTTP %d: an answer that is not JSON", status)
	}
	if status != http.StatusOK {
		return credentials{}, fmt.Errorf("pod identity: refused: HTTP %d: %s %s", status, r.Code, short(r.Message))
	}
	c := credentials{AccessKeyID: r.AccessKeyID, SecretAccessKey: r.SecretAccessKey, SessionToken: r.Token}
	if t, err := time.Parse(time.RFC3339, r.Expiration); err == nil {
		c.Expires = t
	}
	if !c.valid() {
		return credentials{}, errors.New("pod identity: an answer without credentials")
	}
	return c, nil
}

// do sends req with client and reads at most maxResponse of the answer. Transport errors carry
// neither the URL nor anything of the request.
func (w *Wrapper) do(client *http.Client, req *http.Request) (int, []byte, error) {
	ctx := req.Context()
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, fmt.Errorf("no answer: %w", ctx.Err())
		}
		return 0, nil, errors.New("no answer: " + scrubURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return 0, nil, fmt.Errorf("reading the answer: %w", err)
	}
	return resp.StatusCode, raw, nil
}

// short is an error message from AWS, cut to a length fit for a log line.
func short(m string) string {
	if len(m) > 200 {
		return m[:200]
	}
	return m
}
