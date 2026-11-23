// SPDX-License-Identifier: Apache-2.0

package approvals

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/stateracloud/koinon/internal/githubtools"
)

// FooterMarker starts the footer Koinon appends to the body of every pull request an agent opens
// (ADR 0005, amendment 2026-11-23). A pull request body is the agent's text; the footer after the
// marker is Koinon's. The marker is an HTML comment, so the code host does not show it.
const FooterMarker = "<!-- koinon:footer -->"

// cutNote replaces the end of a body that was cut to leave room for the footer.
const cutNote = "\n\n_(Koinon cut the description here to fit the code host's limit.)_"

// Footer is who and what a pull request footer names.
type Footer struct {
	// RequestedBy is the person (or "Koinon", for a run no person started) who asked for the work.
	RequestedBy string
	// ApprovedBy is the person who approved the call, or the reason no person had to.
	ApprovedBy string
	// RunID is the run that opened the pull request.
	RunID string
	// RunURL is the Koinon page of the run; empty when Koinon has no public URL.
	RunURL string
	// ApprovalID is the approval request the pull request came from.
	ApprovalID string
}

// String is the footer in Markdown, starting with FooterMarker.
func (f Footer) String() string {
	var b strings.Builder
	b.WriteString(FooterMarker)
	b.WriteString("\n---\n")
	b.WriteString("A Koinon agent opened this pull request.\n\n")
	fmt.Fprintf(&b, "- Requested by: %s\n", footerText(f.RequestedBy))
	fmt.Fprintf(&b, "- Approved by: %s\n", footerText(f.ApprovedBy))
	run := "`" + footerText(f.RunID) + "`"
	if f.RunURL != "" {
		run = "[" + run + "](" + f.RunURL + ")"
	}
	fmt.Fprintf(&b, "- Run: %s\n", run)
	fmt.Fprintf(&b, "- Approval: `%s`\n", footerText(f.ApprovalID))
	return b.String()
}

// footerText makes a name safe for one Markdown list item: one line, and no character that starts
// a link, an HTML tag or a mention. A display name is user input.
func footerText(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		case '<', '>', '[', ']', '(', ')', '`', '@', '*', '_', '|':
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// RunURL is the Koinon page of a run's task. publicURL is the origin browsers reach Koinon at; an
// empty or malformed one gives "" (the footer then names the run without a link).
func RunURL(publicURL, taskID string) string {
	u, err := url.Parse(strings.TrimSpace(publicURL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + strings.TrimSuffix(u.EscapedPath(), "/") + "/tasks/" + url.PathEscape(taskID)
}

// PullRequestBody is the agent's body with the footer appended. A footer marker in the agent's own
// text is removed, so only Koinon's footer carries it. The body is cut, if needed, so the whole fits
// the code host's limit.
func PullRequestBody(body string, f Footer) string {
	body = strings.TrimSpace(strings.ReplaceAll(body, FooterMarker, ""))
	footer := f.String()
	room := githubtools.MaxBodyBytes - len(footer) - len("\n\n")
	if len(body) > room {
		cut := room - len(cutNote)
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
		body = strings.TrimSpace(body[:max(cut, 0)]) + cutNote
	}
	if body == "" {
		return footer
	}
	return body + "\n\n" + footer
}
