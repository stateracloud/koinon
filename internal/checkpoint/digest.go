// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrDigestMismatch is returned by a restore when the restored files do not match the digest that
// was recorded for the checkpoint. Nothing is sent into the sandbox then.
var ErrDigestMismatch = errors.New("checkpoint: the restored home does not match its recorded digest")

// digestVersion is the first line of the digested text. A change to what the digest covers changes
// it, so an old digest never matches a new computation by accident.
const digestVersion = "koinon-home-checkpoint/v1"

// TreeDigest returns the SHA-256 digest of the files below dir as "sha256:<hex>". It covers every
// entry's name, kind and permission bits, a regular file's size and content, and a symbolic link's
// target. It leaves out times and owners, which a restore does not keep exactly. The result does
// not depend on the order of the directory reads. It reads a file or a directory whose owner bits
// forbid it by widening them for the read and putting them back, because the staged tree keeps the
// modes of the home (see Stage).
func TreeDigest(dir string) (string, error) {
	h := sha256.New()
	_, _ = io.WriteString(h, digestVersion+"\n")
	if err := digestDir(h, dir, ""); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// ValidDigest reports whether s has the form TreeDigest returns.
func ValidDigest(s string) bool {
	hexPart, ok := strings.CutPrefix(s, "sha256:")
	if !ok || len(hexPart) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(hexPart)
	return err == nil
}

func digestDir(h io.Writer, abs, rel string) error {
	info, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	if mode := info.Mode().Perm(); mode&0o700 != 0o700 {
		if err := os.Chmod(abs, mode|0o700); err != nil {
			return err
		}
		defer func() { _ = os.Chmod(abs, mode) }()
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		name := e.Name()
		if rel != "" {
			name = rel + "/" + name
		}
		p := filepath.Join(abs, e.Name())
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			_, _ = fmt.Fprintf(h, "d %q %04o\n", name, fi.Mode().Perm())
			if err := digestDir(h, p, name); err != nil {
				return err
			}
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(h, "l %q %q\n", name, target)
		case fi.Mode().IsRegular():
			sum, err := fileSum(p, fi.Mode().Perm())
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(h, "f %q %04o %d %s\n", name, fi.Mode().Perm(), fi.Size(), sum)
		}
	}
	return nil
}

func fileSum(p string, mode fs.FileMode) (string, error) {
	if mode&0o400 == 0 {
		if err := os.Chmod(p, mode|0o400); err != nil {
			return "", err
		}
		defer func() { _ = os.Chmod(p, mode) }()
	}
	f, err := os.Open(p) //nolint:gosec // below the scratch directory
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	if err = errors.Join(err, f.Close()); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
