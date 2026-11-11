// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/stateracloud/koinon/internal/acp"

	"github.com/stateracloud/koinon/internal/db/store"
)

// Failure classes (ADR 0030, amendment of 2026-10-26). A class is set where the error is first
// understood; an error nobody understands is fatal. Retries and the message to the person follow
// from the class: transient and throttled are retried (backoff, or the provider's Retry-After),
// every other class is not.
const (
	classTransient       = store.RunFailureClassTransient
	classThrottled       = store.RunFailureClassThrottled
	classContextOverflow = store.RunFailureClassContextOverflow
	classBudget          = store.RunFailureClassBudget
	classPolicy          = store.RunFailureClassPolicy
	classFatal           = store.RunFailureClassFatal
	// classLoop: the agent kept repeating itself after the one nudge (loopguard); not retried.
	classLoop = store.RunFailureClassLoop
	// classSandboxUnavailable: the sandbox never became ready within the org's cap, or is in a state
	// the reconciler will not repair (error, deleted or retained, with the agent unhealthy); not retried.
	classSandboxUnavailable = store.RunFailureClassSandboxUnavailable
)

// classified is what a harness's error says about itself.
type classified struct {
	class store.RunFailureClass
	// retryAfter is the provider's Retry-After, zero when it sent none.
	retryAfter time.Duration
	// message tells the person what happened, without the raw error.
	message string
	// gateway: the model gateway refused the request, with 401 (the key is rejected or revoked)
	// or 429 (a limit). A spent budget revokes the key (ADR 0050), so these are the errors a
	// run sees when a limit stopped it before its cancel arrived (budgetstop.go).
	gateway bool
	// auth: the error says authentication failed, with a 401 or in words alone (OpenCode reports a
	// provider's 401 and 403 alike as "Authentication required"). Which party refused, the gateway
	// or the provider, is not in the error; the gateway's record says (providerrefusal.go).
	auth bool
}

var (
	statusRe     = regexp.MustCompile(`\b([45]\d\d)\b`)
	retryAfterRe = regexp.MustCompile(`(?i)retry[-_ ]?after["']?\s*[:=]?\s*["']?(\d+(?:\.\d+)?)`)
)

// classifyPromptError classifies the error of a session/prompt that the harness answered (the
// connection is alive). Harnesses report a failed model request as a JSON-RPC error whose message
// and data carry the HTTP status, the provider's error type and the Retry-After header; the
// structured fields win, the text is the fallback.
func classifyPromptError(err error) classified {
	var text string
	status, retryAfter := 0, time.Duration(0)
	var re *sdk.RequestError
	if errors.As(err, &re) {
		text = re.Message
		if re.Data != nil {
			b, _ := json.Marshal(re.Data)
			text += " " + string(b)
			status, retryAfter = structured(re.Data)
		}
	} else {
		text = err.Error()
	}
	low := strings.ToLower(text)
	hasStatus := status != 0
	if !hasStatus {
		if m := statusRe.FindStringSubmatch(low); m != nil {
			status, _ = strconv.Atoi(m[1])
		}
	}
	if retryAfter == 0 {
		if m := retryAfterRe.FindStringSubmatch(low); m != nil {
			if secs, perr := strconv.ParseFloat(m[1], 64); perr == nil {
				retryAfter = time.Duration(secs * float64(time.Second))
			}
		}
	}
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(low, s) {
				return true
			}
		}
		return false
	}
	// 401 is the gateway rejecting the agent's key. Harnesses may report it without the status
	// (OpenCode: "Authentication required: provider authentication required").
	auth := status == 401 || (!hasStatus && has("authentication required", "authentication failure", "invalid credentials"))
	// The class-specific phrases name their class whatever the status says.
	switch {
	case has("context_length_exceeded", "prompt is too long", "maximum context length", "context window"):
		return classified{class: classContextOverflow, message: "The conversation no longer fits the model's context window."}
	case has("insufficient_quota"):
		return classified{class: classBudget, message: "A spend or token limit stopped the run."}
	}
	// A status the harness reported in a structured field decides the rest; the text heuristics
	// (which can match "500 tokens" or "timeout" in a 400's explanation) only run without one.
	switch {
	case status == 402:
		return classified{class: classBudget, message: "A spend or token limit stopped the run."}
	case status == 403:
		return classified{class: classPolicy, message: "A policy denied the request."}
	case auth:
		return classified{class: classFatal, gateway: true, auth: true}
	case status == 429:
		return classified{class: classThrottled, retryAfter: retryAfter, gateway: true, message: "The model provider is rate limiting requests."}
	case status == 500, status == 502, status == 503, status == 504, status == 529:
		return classified{class: classTransient, message: "The model provider had a temporary fault."}
	case hasStatus:
		return classified{class: classFatal}
	}
	switch {
	case auth:
		return classified{class: classFatal, gateway: true, auth: true}
	case has("spend limit", "budget exceeded", "token cap"):
		return classified{class: classBudget, message: "A spend or token limit stopped the run."}
	case has("denied by policy", "policy denied", "blocked by policy"):
		return classified{class: classPolicy, message: "A policy denied the request."}
	case has("rate limit", "rate_limit", "too many requests"):
		return classified{class: classThrottled, retryAfter: retryAfter, gateway: true, message: "The model provider is rate limiting requests."}
	case has("overloaded", "service unavailable", "timed out", "timeout", "connection reset", "unexpected eof", "bad gateway"):
		return classified{class: classTransient, message: "The model provider had a temporary fault."}
	}
	return classified{class: classFatal}
}

// stopFailure maps an ACP stop reason that ends a turn without finishing it to a failure.
// refusal is the model declining (policy); max_tokens and max_turn_requests are limits the turn
// ran into, which Koinon reports as budget (neither is retried: repeating the turn hits the
// same limit).
func stopFailure(stop acp.StopReason) *failure {
	f := &failure{reason: "the turn stopped: " + string(stop), class: classFatal}
	switch stop {
	case acp.StopRefusal:
		f.class = classPolicy
		f.reason = "the model refused to continue (stop reason refusal)"
	case acp.StopMaxTokens:
		f.class = classBudget
		f.reason = "the model reached its output token limit (stop reason max_tokens)"
	case acp.StopMaxTurnRequests:
		f.class = classBudget
		f.reason = "the turn reached its limit of model requests (stop reason max_turn_requests)"
	}
	return f
}

// structured reads the HTTP status and the Retry-After (seconds) that a harness put in an
// error's data, directly or under "headers"/"responseHeaders".
func structured(data any) (status int, retryAfter time.Duration) {
	m, ok := data.(map[string]any)
	if !ok {
		return 0, 0
	}
	for _, k := range []string{"statusCode", "status_code", "status"} {
		if n, ok := number(m[k]); ok {
			status = int(n)
			break
		}
	}
	look := func(m map[string]any) {
		for k, v := range m {
			switch strings.ToLower(k) {
			case "retryafter", "retry_after", "retry-after":
				if n, ok := number(v); ok && n > 0 && retryAfter == 0 {
					retryAfter = time.Duration(n * float64(time.Second))
				}
			}
		}
	}
	look(m)
	for _, k := range []string{"headers", "responseHeaders", "response_headers"} {
		if h, ok := m[k].(map[string]any); ok {
			look(h)
		}
	}
	return status, retryAfter
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	}
	return 0, false
}

// modelNotFound reports whether a harness's error says the gateway does not serve the requested
// model: HTTP 404 or the gateway's model_not_found.
func modelNotFound(err error) bool {
	text, status := err.Error(), 0
	var re *sdk.RequestError
	if errors.As(err, &re) {
		text = re.Message
		if re.Data != nil {
			b, _ := json.Marshal(re.Data)
			text += " " + string(b)
			status, _ = structured(re.Data)
		}
	}
	low := strings.ToLower(text)
	if status == 0 {
		if m := statusRe.FindStringSubmatch(low); m != nil {
			status, _ = strconv.Atoi(m[1])
		}
	}
	return status == 404 || strings.Contains(low, "model_not_found")
}

// classFailure turns an answered session/prompt error into the failure that ends (or retries)
// the turn. Unknown errors keep the plain reason and are fatal.
func classFailure(what string, err error) *failure {
	c := classifyPromptError(err)
	raw := what + ": " + err.Error()
	f := &failure{reason: raw, class: c.class, retryAfter: c.retryAfter, gateway: c.gateway, authLike: c.auth, modelNotFound: modelNotFound(err)}
	if c.message != "" {
		f.reason = c.message + " (" + raw + ")"
	}
	return f
}

// retryNote opens the prompt of a turn repeated after a model request failed.
const retryNote = "[Koinon] A request to the model failed with a temporary error (%s) and is " +
	"retried. Continue the task from where you stopped. Do not repeat actions that already succeeded."

// exhausted words a retryable failure that ended the run. harnessTries is set when the harness was
// the retrying layer (then agentd made no retries of its own): it is the tries Koinon allowed it,
// not the tries it made, which agentd does not see (a refusal ends after one).
func exhausted(f *failure, retries, harnessTries int, maxWait time.Duration) *failure {
	out := *f
	switch {
	case f.class == classThrottled && f.retryAfter > maxWait:
		out.reason = fmt.Sprintf("%s; the provider asked to wait %s, longer than the %s Koinon waits",
			f.reason, f.retryAfter.Round(time.Second), maxWait)
	case harnessTries > 0:
		out.reason = fmt.Sprintf("%s; the harness stopped retrying (Koinon allows it up to %d tries)", f.reason, harnessTries)
	default:
		out.reason = fmt.Sprintf("%s; gave up after %d retries", f.reason, retries)
	}
	return &out
}
