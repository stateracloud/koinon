// SPDX-License-Identifier: Apache-2.0

package route

import (
	"errors"
	"fmt"
	"strings"
)

// Credential schemes: how the gateway sends the value in the Authorization header.
const (
	// SchemeBasic sends "Basic " and the base64 of User ":" value. Git over HTTPS and the forge APIs
	// take a token this way (GitHub with the user "x-access-token", Gitea with any user).
	SchemeBasic = "basic"
	// SchemeBearer sends "Bearer " and the value.
	SchemeBearer = "bearer"
)

// Credential says where the gateway reads one credential that routes name, and how it sends it
// (#1086 part C). It holds no value: File is a sealed envelope (v2, ADR 0097 decision 12) that only
// the gateway key opens.
type Credential struct {
	// Name is the name that routes give in Route.Credential.
	Name string
	// Org and Agent are the scope of the credential. A route that names it must have the same org and
	// the same agent, so one agent's credential never serves another agent's route.
	Org   string
	Agent string
	// File is the path of the sealed envelope in the gateway.
	File string
	// Scheme is SchemeBasic or SchemeBearer. User is the user name of SchemeBasic.
	Scheme string
	User   string
}

// Validate checks one credential on its own.
func (c Credential) Validate() error {
	switch {
	case c.Name == "":
		return errors.New("a credential needs a name")
	case c.Org == "" || c.Agent == "":
		return fmt.Errorf("credential %q: a credential belongs to one org and one agent", c.Name)
	case c.File == "":
		return fmt.Errorf("credential %q: no file", c.Name)
	}
	switch c.Scheme {
	case SchemeBasic:
		if c.User == "" || strings.ContainsAny(c.User, ":\r\n") {
			return fmt.Errorf("credential %q: the basic scheme needs a user without ':' or a line break", c.Name)
		}
	case SchemeBearer:
		if c.User != "" {
			return fmt.Errorf("credential %q: the bearer scheme has no user", c.Name)
		}
	default:
		return fmt.Errorf("credential %q: unknown scheme %q", c.Name, c.Scheme)
	}
	return nil
}

// CheckCredentials checks a set of credentials against the routes that name them: each credential is
// valid and named once, and each route that names one of them has its org and agent. A route that
// names a credential which is not in the set is not an error: the gateway refuses it
// (credential_unavailable), as it does while a new credential file is still reaching the gateway.
func CheckCredentials(routes []Route, creds []Credential) (map[string]Credential, error) {
	out := make(map[string]Credential, len(creds))
	for _, c := range creds {
		if err := c.Validate(); err != nil {
			return nil, err
		}
		if _, dup := out[c.Name]; dup {
			return nil, fmt.Errorf("credential %q is named twice", c.Name)
		}
		out[c.Name] = c
	}
	for _, r := range routes {
		if r.Credential == "" {
			continue
		}
		c, ok := out[r.Credential]
		if !ok {
			continue
		}
		if r.Org != c.Org || r.Agent != c.Agent {
			return nil, fmt.Errorf("route %q (org %q, agent %q) names credential %q of org %q, agent %q: a credential serves its own agent only",
				r.Name, r.Org, r.Agent, c.Name, c.Org, c.Agent)
		}
	}
	return out, nil
}
