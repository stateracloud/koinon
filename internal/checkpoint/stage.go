// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Limits bound what a tar stream may stage.
type Limits struct {
	// MaxBytes is the total size of the files; MaxEntries the number of entries.
	MaxBytes   int64
	MaxEntries int
}

// DefaultLimits are 4 GiB and two million entries.
var DefaultLimits = Limits{MaxBytes: 4 << 30, MaxEntries: 2_000_000}

// StageStats is what a stream held.
type StageStats struct {
	Bytes, Files, Dirs, Symlinks int64
	// Skipped counts entries that are not saved: excluded paths, hard links, devices, pipes.
	Skipped int64
}

// ErrTooLarge is returned when the stream goes over its limits.
var ErrTooLarge = errors.New("the home is larger than the checkpoint limit")

// Stage extracts the tar stream into dir (which must exist and be empty) and returns what it
// held. Every file operation goes through an os.Root, so no entry, and no symbolic link an entry
// created, can reach outside dir. Owners are ignored (the writer is not root). Permission bits
// are kept; the owner's rwx bits are widened while extracting and the original modes applied
// afterwards.
func Stage(r io.Reader, dir string, lim Limits) (StageStats, error) {
	var st StageStats
	root, err := os.OpenRoot(dir)
	if err != nil {
		return st, err
	}
	defer func() { _ = root.Close() }()
	type fix struct {
		name string
		mode fs.FileMode
		mod  time.Time
	}
	var fixes []fix
	tr := tar.NewReader(r)
	var entries int
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return st, fmt.Errorf("reading the tar stream: %w", err)
		}
		entries++
		if lim.MaxEntries > 0 && entries > lim.MaxEntries {
			return st, fmt.Errorf("%w: more than %d entries", ErrTooLarge, lim.MaxEntries)
		}
		name, ok := cleanName(h.Name)
		if !ok {
			return st, fmt.Errorf("the tar stream has an entry outside the home: %q", h.Name)
		}
		if name == "." {
			continue
		}
		if Excluded(name) {
			st.Skipped++
			continue
		}
		perm := fs.FileMode(h.Mode & 0o777) //nolint:gosec // masked to the permission bits
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o700); err != nil {
				return st, err
			}
			st.Dirs++
			fixes = append(fixes, fix{name, perm | fs.ModeDir, h.ModTime})
		case tar.TypeReg:
			if h.Size < 0 || (lim.MaxBytes > 0 && st.Bytes+h.Size > lim.MaxBytes) {
				return st, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, lim.MaxBytes)
			}
			if err := root.MkdirAll(path.Dir(name), 0o700); err != nil {
				return st, err
			}
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
			if err != nil {
				return st, err
			}
			n, err := io.Copy(f, io.LimitReader(tr, h.Size))
			cerr := f.Close()
			if err = errors.Join(err, cerr); err != nil {
				return st, err
			}
			st.Bytes += n
			st.Files++
			fixes = append(fixes, fix{name, perm, h.ModTime})
		case tar.TypeSymlink:
			if h.Linkname == "" || strings.ContainsRune(h.Linkname, 0) {
				st.Skipped++
				continue
			}
			if err := root.MkdirAll(path.Dir(name), 0o700); err != nil {
				return st, err
			}
			_ = root.Remove(name)
			if err := root.Symlink(h.Linkname, name); err != nil {
				return st, err
			}
			st.Symlinks++
		default: // hard links, devices, pipes, sockets
			st.Skipped++
		}
	}
	// Directories last and deepest first, so a read-only directory does not stop its children.
	sort.Slice(fixes, func(i, j int) bool { return len(fixes[i].name) > len(fixes[j].name) })
	for _, f := range fixes {
		if err := root.Chmod(f.name, f.mode.Perm()); err != nil {
			return st, err
		}
		if !f.mod.IsZero() {
			_ = root.Chtimes(f.name, f.mod, f.mod)
		}
	}
	return st, nil
}

// cleanName cleans a tar entry name to a path relative to the home, or reports that it leaves it.
func cleanName(n string) (string, bool) {
	if n == "" || strings.ContainsRune(n, 0) || strings.HasPrefix(n, "/") {
		return "", false
	}
	c := path.Clean(n)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", false
	}
	return c, true
}

// WriteTar writes the files below dir as a tar stream, names relative to dir. Owners are not
// written (the sandbox's tar runs as the sandbox user and extracts as itself).
func WriteTar(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && !info.IsDir() && info.Mode()&fs.ModeSymlink == 0 {
			return nil
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			h.Name += "/"
		}
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p) //nolint:gosec // below the scratch directory
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		return errors.Join(err, f.Close())
	})
	return errors.Join(err, tw.Close())
}

// RemoveAll deletes a scratch directory, including the read-only parts Stage may have left.
func RemoveAll(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700) //nolint:gosec // a scratch directory of this process
		}
		return nil
	})
	_ = os.RemoveAll(dir)
}
