// SPDX-License-Identifier: Apache-2.0

package codehost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Polled resources (ADR 0066, amendment of 2026-11-12). A resource is one list on the provider that
// one shared poller per (org, connection, resource) watches for every trigger on it. Its string
// form, kind:owner/name, is the poller's key and part of every dedupe key it produces, so it must
// stay stable.
const (
	// ResourceRepoEvents is a repository's activity feed (GitHub GET /repos/{o}/{r}/events), the
	// feed GitHub meant for ETag polling.
	ResourceRepoEvents = "repo_events"
	// ResourceIssueComments is the comments on every issue and pull request of a repository, newest
	// first by creation.
	ResourceIssueComments = "issue_comments"
)

// Resource is one polled list of one repository.
type Resource struct {
	Kind string
	Repo Repo
}

// String is kind:owner/name, the normalized form stored as the poller's key.
func (r Resource) String() string { return r.Kind + ":" + r.Repo.String() }

// ParseResource parses kind:owner/name. The repository part is lower-cased, since both providers
// match repository names without regard to case, so two triggers spelling one repository
// differently share one poller.
func ParseResource(s string) (Resource, error) {
	kind, full, ok := strings.Cut(s, ":")
	if !ok {
		return Resource{}, fmt.Errorf("codehost: resource %q is not kind:owner/name", s)
	}
	switch kind {
	case ResourceRepoEvents, ResourceIssueComments:
	default:
		return Resource{}, fmt.Errorf("codehost: resource %q: unknown kind %q", s, kind)
	}
	owner, name, ok := strings.Cut(strings.ToLower(full), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return Resource{}, fmt.Errorf("codehost: resource %q is not kind:owner/name", s)
	}
	return Resource{Kind: kind, Repo: Repo{Owner: owner, Name: name}}, nil
}

// Validator kinds: what a poll sends to learn that nothing changed without reading the list.
const (
	// ValidatorNone: no validator yet (the first poll) or the provider sent none.
	ValidatorNone = "none"
	// ValidatorETag is sent back as If-None-Match.
	ValidatorETag = "etag"
	// ValidatorLastModified is sent back as If-Modified-Since (GitHub notifications, F5).
	ValidatorLastModified = "last_modified"
	// ValidatorPageHash is Koinon's own: a hash of the first page, for a provider that sends no
	// HTTP validator on lists (Gitea). The page is read, but an identical one ends the poll as
	// unchanged before any trigger work.
	ValidatorPageHash = "page_hash"
)

// Validator is the stored validator of a poll.
type Validator struct {
	Kind  string
	Value string
}

// PollState is what the poller stored for a resource after its last poll.
type PollState struct {
	Validator Validator
	// Cursor is opaque to the poller: each provider adapter writes and reads its own form (an event
	// ID, a comment ID). Empty before the baseline.
	Cursor string
	// Baselined is false until the first successful poll recorded a cursor. The first poll
	// delivers nothing: what happened before the trigger existed is not an event for it.
	Baselined bool
}

// PollItem is one new thing on a polled resource.
type PollItem struct {
	// Key is stable at the provider: the same change always has the same key (a GitHub event ID, a
	// comment ID), so a re-poll, a restart or a second replica dedupe it (durable.SourcePoll).
	Key string
	// Kind is the provider's name for the item: a GitHub event type such as PullRequestEvent, or
	// issue_comment.
	Kind string
	// At is when the provider says the item happened.
	At time.Time
	// Payload is the item as the provider sent it (bounded by the adapter).
	Payload []byte
}

// PollResult is the outcome of one poll.
type PollResult struct {
	// Changed is false when the provider said, or the page hash showed, that nothing changed since
	// the stored validator. Items is then empty and the poll ends before any trigger work.
	Changed bool
	// Items are the new items since the stored cursor, oldest first. Empty on the baseline poll.
	Items []PollItem
	// State is the state to store for the next poll.
	State PollState
	// Interval is the provider's requested minimum time before the next poll of this resource
	// (GitHub X-Poll-Interval); zero when the response stated none.
	Interval time.Duration
}

// Poller is implemented by an Access whose provider can poll resources. Poll sends one
// conditional request where the provider supports it and never sleeps: rate limits come back as
// a *RateLimitError for the caller to schedule.
type Poller interface {
	Poll(ctx context.Context, res Resource, st PollState) (PollResult, error)
}

// ErrResourceUnsupported means the provider cannot poll this kind of resource.
var ErrResourceUnsupported = errors.New("codehost: the provider cannot poll this resource")

// RateLimitError is a provider's answer that the credential is rate limited until RetryAt
// (GitHub x-ratelimit-reset or Retry-After). It wraps the host's StatusError.
type RateLimitError struct {
	Err     error
	RetryAt time.Time
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("%v (rate limited until %s)", e.Err, e.RetryAt.UTC().Format(time.RFC3339))
}

// Unwrap returns the host's error.
func (e *RateLimitError) Unwrap() error { return e.Err }

// PollScoper is implemented by a Provider whose connections can be polled: PollScope is the least
// scope that reads the resource, the one the poller opens the connection with.
type PollScoper interface {
	PollScope(res Resource) (Scope, error)
}
