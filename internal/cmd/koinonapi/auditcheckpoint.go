// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/db/store"
)

// checkpointCommands are the commands that read signed checkpoints (ADR 0100 decision 4.2): the newest
// signed note, and the public keys that verify notes. The signing key itself never leaves the Secret.
func checkpointCommands() []*cobra.Command {
	var dbURL, out string
	checkpoint := &cobra.Command{
		Use:   "checkpoint",
		Short: "Print the newest signed audit checkpoint (ADR 0100 decision 4.2)",
		Long: "Prints the newest signed checkpoint note that koinon-api recorded in the install chain. Copy it, " +
			"and an export of the install chain, to a place outside the cluster and keep it there: a chain that is " +
			"rewritten later no longer matches it (koinon-api audit verify --file <export> --checkpoint <note> " +
			"--public-key <key>). Exits non-zero while there is no checkpoint yet. Run it in a koinon-api pod.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pool, err := pgxpool.New(cmd.Context(), dbURL)
			if err != nil {
				return fmt.Errorf("connect to postgres: %w", err)
			}
			defer pool.Close()
			note, ok, err := audit.LatestCheckpoint(cmd.Context(), store.New(pool))
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("no checkpoint is recorded yet: the first one follows the first anchor (koinon-api audit anchor)")
			}
			return writeOut(cmd.OutOrStdout(), out, note)
		},
	}
	checkpoint.Flags().StringVar(&dbURL, "database-url", "", "Postgres connection string")
	checkpoint.Flags().StringVar(&out, "out", "-", "write to this new file instead of stdout")
	cli.EnvAlias(checkpoint, "database-url", "DATABASE_URL")
	mustRequire(checkpoint, "database-url")

	var keysURL, keysOut string
	keys := &cobra.Command{
		Use:   "public-keys",
		Short: "Print the public keys that verify audit checkpoints, oldest first",
		Long: "Prints one line per signing key koinon-api published as an audit.signing_key.added event. Keep each " +
			"line outside the cluster when the key is new: a verifier trusts only the keys it pinned then, not what " +
			"the database says later. A rotated key stays valid for the checkpoints it signed. Run it in a koinon-api pod.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pool, err := pgxpool.New(cmd.Context(), keysURL)
			if err != nil {
				return fmt.Errorf("connect to postgres: %w", err)
			}
			defer pool.Close()
			ks, err := audit.SigningKeys(cmd.Context(), store.New(pool))
			if err != nil {
				return err
			}
			if len(ks) == 0 {
				return errors.New("no signing key is published yet: the first checkpoint publishes it")
			}
			var b strings.Builder
			for _, k := range ks {
				b.WriteString(k.String() + "\n")
			}
			return writeOut(cmd.OutOrStdout(), keysOut, []byte(b.String()))
		},
	}
	keys.Flags().StringVar(&keysURL, "database-url", "", "Postgres connection string")
	keys.Flags().StringVar(&keysOut, "out", "-", "write to this new file instead of stdout")
	cli.EnvAlias(keys, "database-url", "DATABASE_URL")
	mustRequire(keys, "database-url")
	return []*cobra.Command{checkpoint, keys}
}

// writeOut writes b to stdout, or to a new file (it never replaces one).
func writeOut(stdout io.Writer, path string, b []byte) error {
	if path == "" || path == "-" {
		_, err := stdout.Write(b)
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the operator names the output file
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	_, err = f.Write(b)
	if cerr := f.Close(); cerr != nil && err == nil {
		err = fmt.Errorf("close %s: %w", path, cerr)
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

// readCheckpoint reads the note file and checks its signature against the public keys the operator gave
// (a key line, or a file of key lines). It prints the verdict on the signature. An unknown key, a changed
// note or a note of a missing file is an error: nothing can be pinned to it.
func readCheckpoint(path string, keyArgs []string, out io.Writer) (audit.Note, error) {
	if len(keyArgs) == 0 {
		return audit.Note{}, errors.New("--checkpoint needs at least one --public-key: a note proves nothing without the key kept off the cluster")
	}
	if path == "-" {
		return audit.Note{}, errors.New("--checkpoint needs a file path: standard input is not supported")
	}
	f, err := os.Open(path) //nolint:gosec // the operator names the file to check
	if err != nil {
		return audit.Note{}, fmt.Errorf("--checkpoint: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20))
	_ = f.Close()
	if err != nil {
		return audit.Note{}, fmt.Errorf("--checkpoint: %w", err)
	}
	var keys []audit.PublicKey
	for _, a := range keyArgs {
		ks, err := loadPublicKeys(a)
		if err != nil {
			return audit.Note{}, fmt.Errorf("--public-key %s: %w", a, err)
		}
		keys = append(keys, ks...)
	}
	n, k, err := audit.VerifyNote(raw, keys)
	if err != nil {
		return audit.Note{}, fmt.Errorf("--checkpoint %s: %w", path, err)
	}
	_, _ = fmt.Fprintf(out, "ok      checkpoint %s is signed by key %s: install chain_seq %d\n", path, k.ID(), n.ChainSeq)
	return n, nil
}

// loadPublicKeys reads a key line, or the key lines of a file (blank lines and lines that start with # are skipped).
func loadPublicKeys(arg string) ([]audit.PublicKey, error) {
	text := arg
	if b, err := os.ReadFile(arg); err == nil { //nolint:gosec // the operator names the key file
		text = string(b)
	} else if !errors.Is(err, os.ErrNotExist) && !strings.Contains(arg, "+") {
		return nil, err
	}
	var out []audit.PublicKey
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		k, err := audit.ParsePublicKey(l)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	if len(out) == 0 {
		return nil, errors.New("no key found")
	}
	return out, nil
}
