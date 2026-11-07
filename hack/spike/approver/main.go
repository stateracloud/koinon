// SPDX-License-Identifier: Apache-2.0

// Command approver is throwaway spike code for step 6 of docs/spikes/openshell.md:
// an MCP server behind agentgateway whose create_pull_request tool only records an
// approval request ("park"). A human approves the exact request over a separate
// admin port; the stub then creates exactly that pull request on Gitea, once.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const gitea = "https://git.example.invalid/api/v1"

// PullRequest is the exact call an agent asks for; its hash binds the approval.
type PullRequest struct {
	Owner string `json:"owner" jsonschema:"repository owner"`
	Repo  string `json:"repo" jsonschema:"repository name"`
	Head  string `json:"head" jsonschema:"source branch"`
	Base  string `json:"base" jsonschema:"target branch"`
	Title string `json:"title" jsonschema:"pull request title"`
	Body  string `json:"body,omitempty" jsonschema:"pull request description"`
	// Files are committed to head (created from base if missing) on approval, so the
	// agent needs no direct write access to the repository.
	Files []File `json:"files,omitempty" jsonschema:"files to add on the head branch before the pull request is opened"`
}

// File is one file the approved call writes.
type File struct {
	Path    string `json:"path" jsonschema:"path in the repository, for example hello.md"`
	Content string `json:"content" jsonschema:"full file content"`
}

func (p PullRequest) hash() string {
	b, _ := json.Marshal(p) // struct field order makes this canonical
	s := sha256.Sum256(append([]byte("create_pull_request\x00"), b...))
	return hex.EncodeToString(s[:])
}

type request struct {
	ID        string      `json:"id"`
	Tool      string      `json:"tool"`
	Args      PullRequest `json:"args"`
	ArgsHash  string      `json:"args_sha256"`
	Status    string      `json:"status"` // pending, executed, failed
	PRURL     string      `json:"pr_url,omitempty"`
	Created   time.Time   `json:"created"`
	Executed  int         `json:"executions"`
	LastError string      `json:"last_error,omitempty"`
}

type store struct {
	mu   sync.Mutex
	reqs map[string]*request
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	st := &store{reqs: map[string]*request{}}
	token := os.Getenv("GITEA_TOKEN")

	srv := mcp.NewServer(&mcp.Implementation{Name: "koinon-spike-approver", Version: "0.0.1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "create_pull_request",
		Description: "Open a pull request. Needs human approval: returns a request id immediately; the pull request is created only after approval.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in PullRequest) (*mcp.CallToolResult, any, error) {
		h := in.hash()
		st.mu.Lock()
		r, ok := st.reqs[h[:16]]
		if !ok {
			r = &request{ID: h[:16], Tool: "create_pull_request", Args: in, ArgsHash: h, Status: "pending", Created: time.Now().UTC()}
			st.reqs[r.ID] = r
		}
		snapshot := *r
		st.mu.Unlock()
		log.Info("approval requested", "id", snapshot.ID, "tool", snapshot.Tool, "args", snapshot.Args, "args_sha256", snapshot.ArgsHash, "target", "gitea:"+in.Owner+"/"+in.Repo, "status", snapshot.Status)
		return text(fmt.Sprintf("approval requested: id=%s status=%s. The run should park until a human decides.", snapshot.ID, snapshot.Status)), nil, nil
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_approval_status",
		Description: "Status of an approval request and, once executed, the pull request URL.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in struct {
		ID string `json:"id"`
	},
	) (*mcp.CallToolResult, any, error) {
		st.mu.Lock()
		defer st.mu.Unlock()
		r, ok := st.reqs[in.ID]
		if !ok {
			return text("unknown request " + in.ID), nil, nil
		}
		return text(fmt.Sprintf("id=%s status=%s pr=%s", r.ID, r.Status, r.PRURL)), nil, nil
	})
	// Stands in for an upstream tool that would write without approval; agentgateway's
	// CEL rules must hide it. Reaching it is a spike failure, so it only logs loudly.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "direct_create_pull_request",
		Description: "Create a pull request immediately (no approval).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in PullRequest) (*mcp.CallToolResult, any, error) {
		log.Error("BYPASS TOOL REACHED", "args", in)
		return text("bypass tool reached"), nil, nil
	})

	mcpMux := http.NewServeMux()
	mcpMux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))

	admin := http.NewServeMux()
	admin.HandleFunc("GET /requests", func(w http.ResponseWriter, _ *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		_ = json.NewEncoder(w).Encode(st.reqs)
	})
	admin.HandleFunc("POST /approve", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID       string `json:"id"`
			ArgsHash string `json:"args_sha256"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		req, ok := st.reqs[in.ID]
		switch {
		case !ok:
			http.Error(w, "unknown request", http.StatusNotFound)
			return
		case in.ArgsHash != req.ArgsHash:
			log.Warn("approval rejected: arguments differ from the request", "id", in.ID, "approved_sha256", in.ArgsHash, "requested_sha256", req.ArgsHash)
			http.Error(w, "approval does not match the requested arguments", http.StatusConflict)
			return
		case req.Status == "executed":
			log.Info("approval replayed; already executed", "id", req.ID, "pr", req.PRURL)
			_ = json.NewEncoder(w).Encode(req)
			return
		}
		url, err := createPR(r.Context(), token, req)
		req.Executed++
		if err != nil {
			req.Status, req.LastError = "failed", err.Error()
			log.Error("execution failed", "id", req.ID, "err", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		req.Status, req.PRURL = "executed", url
		log.Info("executed approved call", "id", req.ID, "pr", url, "args_sha256", req.ArgsHash)
		_ = json.NewEncoder(w).Encode(req)
	})

	go func() { log.Error("admin", "err", http.ListenAndServe(":9090", admin)) }()
	log.Error("mcp", "err", http.ListenAndServe(":8080", mcpMux))
}

// createPR is idempotent per request: it creates head and the files only when they
// are missing, and looks for an open pull request with the same head and base first.
func createPR(ctx context.Context, token string, r *request) (string, error) {
	a := r.Args
	if len(a.Files) > 0 {
		if _, err := call(ctx, token, http.MethodGet, fmt.Sprintf("/repos/%s/%s/branches/%s", a.Owner, a.Repo, a.Head), nil); err != nil {
			b, _ := json.Marshal(map[string]string{"new_branch_name": a.Head, "old_ref_name": a.Base})
			if _, err := call(ctx, token, http.MethodPost, fmt.Sprintf("/repos/%s/%s/branches", a.Owner, a.Repo), b); err != nil {
				return "", err
			}
		}
		for _, f := range a.Files {
			if _, err := call(ctx, token, http.MethodGet, fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s", a.Owner, a.Repo, f.Path, a.Head), nil); err == nil {
				continue
			}
			b, _ := json.Marshal(map[string]string{
				"branch": a.Head, "message": "add " + f.Path + " (approved request " + r.ID + ")",
				"content": base64.StdEncoding.EncodeToString([]byte(f.Content)),
			})
			if _, err := call(ctx, token, http.MethodPost, fmt.Sprintf("/repos/%s/%s/contents/%s", a.Owner, a.Repo, f.Path), b); err != nil {
				return "", err
			}
		}
	}
	list, err := call(ctx, token, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls?state=open", a.Owner, a.Repo), nil)
	if err != nil {
		return "", err
	}
	var open []struct {
		HTMLURL string               `json:"html_url"`
		Head    struct{ Ref string } `json:"head"`
		Base    struct{ Ref string } `json:"base"`
	}
	_ = json.Unmarshal(list, &open)
	for _, p := range open {
		if p.Head.Ref == a.Head && p.Base.Ref == a.Base {
			return p.HTMLURL, nil
		}
	}
	body, _ := json.Marshal(map[string]string{
		"head": a.Head, "base": a.Base, "title": a.Title,
		"body": a.Body + "\n\nApproved Koinon request " + r.ID + " (sha256 " + r.ArgsHash + ")",
	})
	out, err := call(ctx, token, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls", a.Owner, a.Repo), body)
	if err != nil {
		return "", err
	}
	var created struct {
		HTMLURL string `json:"html_url"`
	}
	_ = json.Unmarshal(out, &created)
	return created.HTMLURL, nil
}

func call(ctx context.Context, token, method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, gitea+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gitea %s %s: %d %s", method, path, resp.StatusCode, buf.String())
	}
	return buf.Bytes(), nil
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}
