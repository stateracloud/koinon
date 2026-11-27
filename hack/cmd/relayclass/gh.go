// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const ghTimeout = time.Minute

// ghClient reads GitHub through `gh api` (REST only). GH overrides the gh command.
type ghClient struct{ bin string }

func newGHClient() *ghClient {
	bin := os.Getenv("GH")
	if bin == "" {
		bin = "gh"
	}
	return &ghClient{bin: bin}
}

func (g *ghClient) api(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, g.bin, append([]string{"api"}, args...)...) //nolint:gosec // the gh command; arguments are built here
	cmd.Env = append(os.Environ(), "GH_PAGER=cat", "PAGER=cat", "GH_PROMPT_DISABLED=1")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh api %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

// pages decodes the output of `gh api --paginate`: one JSON value per page, written one after another.
func pages[T any](raw []byte) ([]T, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var all []T
	for {
		var page T
		if err := dec.Decode(&page); err != nil {
			if errors.Is(err, io.EOF) {
				return all, nil
			}
			return nil, err
		}
		all = append(all, page)
	}
}

func (g *ghClient) PR(repo string, n int) (PR, error) {
	raw, err := g.api(fmt.Sprintf("repos/%s/pulls/%d", repo, n))
	if err != nil {
		return PR{}, err
	}
	var v struct {
		Number int    `json:"number"`
		State  string `json:"state"`
		Merged bool   `json:"merged"`
		Draft  bool   `json:"draft"`
		Body   string `json:"body"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return PR{}, fmt.Errorf("pull request %d: %w", n, err)
	}
	return PR{Number: v.Number, State: v.State, Merged: v.Merged, Draft: v.Draft, HeadSHA: v.Head.SHA, Body: v.Body}, nil
}

type apiComment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

func (c apiComment) comment() Comment { return Comment(c) }

func (g *ghClient) Comments(repo string, n int) ([]Comment, error) {
	raw, err := g.api("--paginate", fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", repo, n))
	if err != nil {
		return nil, err
	}
	ps, err := pages[[]apiComment](raw)
	if err != nil {
		return nil, fmt.Errorf("comments of %d: %w", n, err)
	}
	var out []Comment
	for _, p := range ps {
		for _, c := range p {
			out = append(out, c.comment())
		}
	}
	return out, nil
}

func (g *ghClient) Comment(repo string, id int64) (Comment, error) {
	raw, err := g.api(fmt.Sprintf("repos/%s/issues/comments/%d", repo, id))
	if err != nil {
		return Comment{}, err
	}
	var c apiComment
	if err := json.Unmarshal(raw, &c); err != nil {
		return Comment{}, fmt.Errorf("comment %d: %w", id, err)
	}
	return c.comment(), nil
}

func (g *ghClient) CheckRuns(repo, sha string) ([]CheckRun, error) {
	if sha == "" {
		return nil, nil
	}
	raw, err := g.api("--paginate", fmt.Sprintf("repos/%s/commits/%s/check-runs?per_page=100", repo, sha))
	if err != nil {
		return nil, err
	}
	type page struct {
		CheckRuns []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	ps, err := pages[page](raw)
	if err != nil {
		return nil, fmt.Errorf("check runs of %s: %w", sha, err)
	}
	var out []CheckRun
	for _, p := range ps {
		for _, r := range p.CheckRuns {
			out = append(out, CheckRun{ID: r.ID, Name: r.Name, Status: r.Status, Conclusion: r.Conclusion})
		}
	}
	return out, nil
}
