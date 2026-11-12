// Package install extracts an addon zip into an AddOns directory.
package install

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/maddawik/wam/internal/errs"
)

// Zip extracts the zip at zipPath into a temp dir beside addonsDir, then
// renames each top-level folder into addonsDir. It returns the folders, in
// the zip's casing. It never overwrites an existing folder except those named
// in owned, which an earlier install put there; those are swapped for the new
// ones, and put back if the swap fails.
func Zip(zipPath, addonsDir string, owned []string) (folders []string, err error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, errs.Wrap(errs.Runtime, err, "downloaded file is not a valid zip", "")
	}
	defer func() { _ = zr.Close() }()

	if err := os.MkdirAll(addonsDir, 0o755); err != nil {
		return nil, errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot create %s", addonsDir), "")
	}
	tmp, err := os.MkdirTemp(filepath.Dir(addonsDir), ".wam-tmp-")
	if err != nil {
		return nil, errs.Wrap(errs.Runtime, err, "cannot create a temp folder beside the AddOns folder", "")
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	folders, err = extract(zr, tmp)
	if err != nil {
		return nil, err
	}
	own := map[string]bool{}
	for _, f := range owned {
		own[f] = true
	}
	for _, f := range folders {
		if _, err := os.Lstat(filepath.Join(addonsDir, f)); err == nil && !own[f] {
			return nil, errs.New(errs.Conflict, fmt.Sprintf("folder %s already exists in %s and wam does not own it", f, addonsDir), "")
		}
	}

	// Move owned folders aside, place the new ones, and undo on failure.
	var aside string
	if len(owned) > 0 {
		aside, err = os.MkdirTemp(filepath.Dir(addonsDir), ".wam-old-")
		if err != nil {
			return nil, errs.Wrap(errs.Runtime, err, "cannot create a temp folder beside the AddOns folder", "")
		}
		defer func() { _ = os.RemoveAll(aside) }()
	}
	var moved, placed []string
	rollback := func() {
		for _, p := range placed {
			_ = os.RemoveAll(filepath.Join(addonsDir, p))
		}
		for _, m := range moved {
			_ = os.Rename(filepath.Join(aside, m), filepath.Join(addonsDir, m))
		}
	}
	for _, f := range owned {
		err := os.Rename(filepath.Join(addonsDir, f), filepath.Join(aside, f))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			rollback()
			return nil, errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot replace %s in %s", f, addonsDir), "")
		}
		moved = append(moved, f)
	}
	for _, f := range folders {
		if err := os.Rename(filepath.Join(tmp, f), filepath.Join(addonsDir, f)); err != nil {
			rollback()
			return nil, errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot place %s in %s", f, addonsDir), "")
		}
		placed = append(placed, f)
	}
	return folders, nil
}

// extract unpacks zr into dir and returns its top-level folders.
func extract(zr *zip.ReadCloser, dir string) ([]string, error) {
	var folders []string
	seen := map[string]bool{}
	for _, f := range zr.File {
		name := strings.TrimSuffix(f.Name, "/")
		if name == "" {
			continue
		}
		if !filepath.IsLocal(filepath.FromSlash(name)) {
			return nil, errs.New(errs.Conflict, fmt.Sprintf("zip entry %q escapes the AddOns folder", f.Name), "")
		}
		top, _, nested := strings.Cut(name, "/")
		if !nested && !f.FileInfo().IsDir() {
			return nil, errs.New(errs.Conflict, fmt.Sprintf("zip has a file %q outside any addon folder", f.Name), "")
		}
		if !seen[top] {
			seen[top] = true
			folders = append(folders, top)
		}
		dest := filepath.Join(dir, filepath.FromSlash(path.Clean(name)))
		if err := extractEntry(f, dest); err != nil {
			return nil, err
		}
	}
	if len(folders) == 0 {
		return nil, errs.New(errs.Conflict, "zip contains no addon folders", "")
	}
	return folders, nil
}

func extractEntry(f *zip.File, dest string) error {
	mode := f.Mode()
	switch {
	case mode.IsDir():
		return os.MkdirAll(dest, 0o755)
	case !mode.IsRegular():
		return errs.New(errs.Conflict, fmt.Sprintf("zip entry %q is not a regular file", f.Name), "")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return errs.New(errs.Conflict, fmt.Sprintf("zip lists %q twice", f.Name), "")
		}
		return err
	}
	_, err = io.Copy(out, rc)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
