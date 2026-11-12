// SPDX-License-Identifier: Apache-2.0

package keyscli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/stateracloud/koinon/internal/secretstore"
)

// ErrCompareFailed is returned by Compare when the key set here does not hold a KEK of the other
// install, or holds it with another fingerprint, or has another active KEK.
var ErrCompareFailed = errors.New("the key set does not match the other install's")

// Compare reads the output of `keys status` of another install from t.In and checks that this key set
// holds every KEK listed there with the same fingerprint, and has the same active KEK. Only IDs and
// fingerprints cross: no key material is exported or written to a file, so it replaces the export
// round trip as the check that a restore into a fresh namespace has the right keys. KEKs only here are
// fine (a new install made its own first KEK). It prints one line per KEK of the other install and
// returns ErrCompareFailed when one is not ok.
func (t *Tool) Compare(ctx context.Context) error {
	if t.In == nil {
		return errors.New("compare reads the output of keys status of the other install from standard input")
	}
	remote, active, err := parseStatus(t.In)
	if err != nil {
		return err
	}
	ks, err := t.Store.Load(ctx)
	if err != nil {
		return err
	}
	defer func() {
		for _, k := range ks.Keys {
			clear(k)
		}
	}()
	bad := 0
	ids := make([]string, 0, len(remote))
	for id := range remote {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		k := ks.Keys[id]
		switch {
		case k == nil:
			bad++
			_, _ = fmt.Fprintf(t.Out, "MISSING  %s (fingerprint %s)\n", id, remote[id])
		case secretstore.Fingerprint(k) != remote[id]:
			bad++
			_, _ = fmt.Fprintf(t.Out, "DIFFERS  %s (here %s, there %s)\n", id, secretstore.Fingerprint(k), remote[id])
		default:
			_, _ = fmt.Fprintf(t.Out, "ok       %s (fingerprint %s)\n", id, remote[id])
		}
	}
	if ks.Active != active {
		bad++
		_, _ = fmt.Fprintf(t.Out, "ACTIVE   here %s, there %s (keys import makes the other install's KEK active)\n", ks.Active, active)
	}
	if bad > 0 {
		return ErrCompareFailed
	}
	_, _ = fmt.Fprintf(t.Out, "The key set holds all %d KEK(s) of the other install and has the same active KEK.\n", len(remote))
	return nil
}

// parseStatus reads the KEK table of `keys status`: id, state, values, wrapped by, fingerprint. MISSING
// rows (a KEK the other install lacks) and everything that is not a table row are skipped.
func parseStatus(r interface{ Read([]byte) (int, error) }) (map[string]string, string, error) {
	remote, active := map[string]string{}, ""
	sc := bufio.NewScanner(r)
	inTable := false
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		switch {
		case len(f) >= 5 && f[0] == "KEK" && f[1] == "STATE":
			inTable = true
			continue
		case len(f) == 0:
			inTable = false // the table ends at the first blank line; the lines after it are prose
		}
		if !inTable || len(f) < 5 || (f[1] != "active" && f[1] != "retained") {
			continue
		}
		remote[f[0]] = f[len(f)-1]
		if f[1] == "active" {
			active = f[0]
		}
	}
	if err := sc.Err(); err != nil {
		return nil, "", err
	}
	if len(remote) == 0 || active == "" {
		return nil, "", errors.New("no KEK table with an active KEK on standard input: pipe in `koinon-api keys status` of the other install")
	}
	return remote, active, nil
}
