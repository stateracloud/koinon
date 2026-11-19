// SPDX-License-Identifier: Apache-2.0

package a2atools

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"strings"

	"github.com/stateracloud/koinon/internal/gateway"
)

// minKeyBytes is the shortest handle key that is accepted.
const minKeyBytes = 16

var errBadHandle = errors.New("a2atools: bad handle")

// handles signs the task and context IDs that a model sees (ADR 0123 decision 10). All agents with a
// grant share the credential of one remote agent, and the remote agent cannot tell them apart. So the
// model gets `<ID>.<tag>` in place of the ID. The tag is an HMAC-SHA256 over the kind, the org, the
// calling agent, the remote agent and the ID. A handle of one agent is no handle of another agent, of
// another remote agent or of another org, and a context ID is no task ID.
type handles struct{ key []byte }

// handleKind keeps the tag of a task ID apart from the tag of a context ID.
type handleKind string

const (
	kindTask    handleKind = "task"
	kindContext handleKind = "context"
)

// readKey reads the key from the file of the target. An error names no content.
func readKey(file string) (handles, error) {
	b, err := os.ReadFile(file) //nolint:gosec // the path is rendered by the reconciler
	if err != nil {
		return handles{}, errors.New("a2atools: the handle key cannot be read")
	}
	k := []byte(strings.TrimRight(string(b), "\r\n"))
	if len(k) < minKeyBytes {
		return handles{}, errors.New("a2atools: the handle key is too short")
	}
	return handles{key: k}, nil
}

func (h handles) tag(kind handleKind, org string, agent gateway.AgentRef, remoteID, id string) string {
	m := hmac.New(sha256.New, h.key)
	// A zero byte separates the fields: none of them has one, so two field lists never give one input.
	for _, f := range []string{"koinon-a2a-handle", string(kind), org, agent.Org, agent.Name, remoteID, id} {
		m.Write([]byte(f))
		m.Write([]byte{0})
	}
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// seal returns the handle for id.
func (h handles) seal(kind handleKind, org string, agent gateway.AgentRef, remoteID, id string) string {
	return id + "." + h.tag(kind, org, agent, remoteID, id)
}

// open returns the ID of a handle, or an error when the tag is wrong for this caller.
func (h handles) open(kind handleKind, org string, agent gateway.AgentRef, remoteID, handle string) (string, error) {
	i := strings.LastIndexByte(handle, '.')
	if i <= 0 {
		return "", errBadHandle
	}
	id, got := handle[:i], handle[i+1:]
	want := h.tag(kind, org, agent, remoteID, id)
	if !hmac.Equal([]byte(got), []byte(want)) {
		return "", errBadHandle
	}
	return id, nil
}
