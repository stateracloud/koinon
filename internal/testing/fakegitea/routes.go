// SPDX-License-Identifier: Apache-2.0

package fakegitea

import (
	"net/http"
	"slices"
	"strings"
)

const api = "/api/v1"

// repoHandler is an endpoint scoped to one repository.
type repoHandler func(w http.ResponseWriter, r *http.Request, rp *repo, login string) error

func (f *Fake) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api+"/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": f.Version})
	})
	mux.HandleFunc("GET "+api+"/user", f.authed(func(w http.ResponseWriter, r *http.Request, login string) error {
		writeJSON(w, http.StatusOK, userJSON(baseURL(r), login))
		return nil
	}))
	mux.HandleFunc("GET "+api+"/user/repos", f.authed(f.listRepos))

	const rr = "GET " + api + "/repos/{owner}/{repo}"
	const rw = "POST " + api + "/repos/{owner}/{repo}"
	mux.HandleFunc(rr, f.scoped(f.getRepo))
	mux.HandleFunc(rr+"/branches", f.scoped(f.listBranches))
	mux.HandleFunc(rr+"/branches/{branch...}", f.scoped(f.getBranch))
	mux.HandleFunc(rw+"/branches", f.scoped(f.createBranch))
	mux.HandleFunc(rr+"/git/commits/{sha}", f.scoped(f.getGitCommit))
	mux.HandleFunc(rr+"/git/trees/{sha}", f.scoped(f.getTree))
	mux.HandleFunc(rr+"/contents", f.scoped(f.getContents))
	mux.HandleFunc(rr+"/contents/{path...}", f.scoped(f.getContents))
	mux.HandleFunc(rw+"/contents", f.scoped(f.changeFiles))
	mux.HandleFunc(rr+"/pulls", f.scoped(f.listPulls))
	mux.HandleFunc(rw+"/pulls", f.scoped(f.createPull))
	mux.HandleFunc(rw+"/issues", f.scoped(f.createIssue))
	mux.HandleFunc(rr+"/pulls/{index}", f.scoped(f.getPull))
	mux.HandleFunc(rr+"/pulls/{base}/{head...}", f.scoped(f.getPullByBaseHead))
	mux.HandleFunc(rr+"/issues/comments", f.scoped(f.listRepoComments))
	mux.HandleFunc(rr+"/issues/{index}/comments", f.scoped(f.listComments))
	mux.HandleFunc(rw+"/issues/{index}/comments", f.scoped(f.createComment))
	mux.HandleFunc(api+"/", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := f.authenticate(r); !ok {
			writeErr(w, errf(http.StatusUnauthorized, "token is required"))
			return
		}
		writeErr(w, errf(http.StatusNotFound, "not found"))
	})
	// Everything else is git smart HTTP: /{owner}/{repo}.git/...
	mux.HandleFunc("/", f.smartHTTP)
	return mux
}

func isWrite(r *http.Request) bool { return r.Method != http.MethodGet && r.Method != http.MethodHead }

func (f *Fake) authed(h func(w http.ResponseWriter, r *http.Request, login string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, login, ok := f.identify(r)
		if !ok {
			writeErr(w, errf(http.StatusUnauthorized, "token is required"))
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		// Gitea checks the token's scope before it reads the request.
		if isWrite(r) && f.scopes[token] == "read" {
			writeErr(w, errf(http.StatusForbidden, "token does not have at least one of required scope(s): [write:repository]"))
			return
		}
		if err := h(w, r, login); err != nil {
			writeErr(w, err)
		}
	}
}

// scoped authenticates and resolves {owner}/{repo}, matching names case-insensitively as Gitea
// does.
func (f *Fake) scoped(h repoHandler) http.HandlerFunc {
	return f.authed(func(w http.ResponseWriter, r *http.Request, login string) error {
		rp, err := f.lookup(r.PathValue("owner"), r.PathValue("repo"))
		if err != nil {
			return err
		}
		perm := f.permission(login, rp.name)
		if perm == "" {
			return errf(http.StatusNotFound, "The target couldn't be found.")
		}
		if isWrite(r) && !canWrite(perm) {
			return errf(http.StatusForbidden, "user should have a permission to write to a repository")
		}
		return h(w, r, rp, login)
	})
}

// lookup finds a repository; the caller holds f.mu.
func (f *Fake) lookup(owner, name string) (*repo, error) {
	if !strings.EqualFold(owner, f.Owner) {
		return nil, errf(http.StatusNotFound, "user redirect does not exist [name: %s]", owner)
	}
	rp, ok := f.repos[strings.ToLower(name)]
	if !ok {
		return nil, errf(http.StatusNotFound, "The target couldn't be found.")
	}
	return rp, nil
}

func (f *Fake) sortedRepos() []*repo {
	rs := make([]*repo, 0, len(f.repos))
	for _, rp := range f.repos {
		rs = append(rs, rp)
	}
	slices.SortFunc(rs, func(a, b *repo) int { return strings.Compare(strings.ToLower(a.name), strings.ToLower(b.name)) })
	return rs
}
