// SPDX-License-Identifier: Apache-2.0

package gitpush

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/go-git/go-git/v5/utils/ioutil"
)

// DialFunc is the egress policy's dial check (catalog.URLPolicy.DialContext).
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// httpTimeout bounds one HTTP exchange of a fetch, a listing or a push.
const httpTimeout = 5 * time.Minute

// newTransport returns the git smart-HTTP transport every gitpush network call uses, in place of
// go-git's process-wide client. It is the shape of the code-host API clients (internal/connectors
// github and gitea, and internal/tracing/export): it connects only through dial when one is set,
// ignores the environment proxy then (a proxy would take the traffic around the dial check) and
// refuses an HTTP redirect instead of following it, so a connection's admin-supplied URL cannot
// send Koinon on to another host.
func newTransport(dial DialFunc) transport.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if dial != nil {
		tr.DialContext = dial
		tr.Proxy = nil
	}
	hc := &http.Client{Transport: tr, Timeout: httpTimeout}
	return githttp.NewClientWithOptions(hc, &githttp.ClientOptions{RedirectPolicy: githttp.NoFollowRedirects})
}

func (a *access) endpoint(url string) (*transport.Endpoint, error) {
	ep, err := transport.NewEndpoint(url)
	if err != nil {
		return nil, err
	}
	ep.CaBundle = a.o.CABundle
	return ep, nil
}

// uploadPack opens an upload-pack session on the remote of a repository.
func (a *access) uploadPack(url string) (transport.UploadPackSession, error) {
	ep, err := a.endpoint(url)
	if err != nil {
		return nil, err
	}
	return a.tr.NewUploadPackSession(ep, a.auth())
}

// listRefs returns the refs the remote advertises.
func (a *access) listRefs(ctx context.Context, url string) (_ []*plumbing.Reference, err error) {
	sess, err := a.uploadPack(url)
	if err != nil {
		return nil, err
	}
	defer ioutil.CheckClose(sess, &err)
	ar, err := sess.AdvertisedReferencesContext(ctx)
	if err != nil {
		return nil, err
	}
	refs, err := ar.AllReferences()
	if err != nil {
		return nil, err
	}
	iter, err := refs.IterReferences()
	if err != nil {
		return nil, err
	}
	var out []*plumbing.Reference
	err = iter.ForEach(func(r *plumbing.Reference) error {
		out = append(out, r)
		return nil
	})
	return out, err
}

// fetchShallow fetches the history of spec at depth 1 into st: a full commit SHA (the host must
// serve commits by SHA) or a full ref name.
func (a *access) fetchShallow(ctx context.Context, st *memory.Storage, url, spec string) (err error) {
	sess, err := a.uploadPack(url)
	if err != nil {
		return err
	}
	defer ioutil.CheckClose(sess, &err)
	ar, err := sess.AdvertisedReferencesContext(ctx)
	if err != nil {
		return err
	}
	var want plumbing.Hash
	if sha1Re.MatchString(spec) {
		if !ar.Capabilities.Supports(capability.AllowReachableSHA1InWant) && !ar.Capabilities.Supports(capability.AllowTipSHA1InWant) {
			return errors.New("gitpush: the host does not serve a commit by its SHA")
		}
		want = plumbing.NewHash(spec)
	} else {
		refs, err := ar.AllReferences()
		if err != nil {
			return err
		}
		ref, err := refs.Reference(plumbing.ReferenceName(spec))
		if err != nil {
			return fmt.Errorf("gitpush: %s: %w", spec, err)
		}
		want = ref.Hash()
	}
	req := packp.NewUploadPackRequestFromCapabilities(ar.Capabilities)
	req.Depth = packp.DepthCommits(1)
	if err := req.Capabilities.Set(capability.Shallow); err != nil {
		return err
	}
	if ar.Capabilities.Supports(capability.NoProgress) {
		if err := req.Capabilities.Set(capability.NoProgress); err != nil {
			return err
		}
	}
	req.Wants = []plumbing.Hash{want}
	resp, err := sess.UploadPack(ctx, req)
	if err != nil {
		return err
	}
	defer ioutil.CheckClose(resp, &err)
	if len(resp.Shallows) > 0 {
		have, err := st.Shallow()
		if err != nil {
			return err
		}
		if err := st.SetShallow(append(have, resp.Shallows...)); err != nil {
			return err
		}
	}
	var body io.Reader = resp
	switch {
	case req.Capabilities.Supports(capability.Sideband):
		body = sideband.NewDemuxer(sideband.Sideband, resp)
	case req.Capabilities.Supports(capability.Sideband64k):
		body = sideband.NewDemuxer(sideband.Sideband64k, resp)
	}
	return packfile.UpdateObjectStorage(st, body)
}
