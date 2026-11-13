// Package install puts addon folders into an AddOns directory and takes them
// out again, so that a failure or crash never leaves a folder half-written.
//
// Replacing folders uses the same sequence on every OS, since Windows can't
// rename over an existing directory: the old folders are renamed into a
// .wam-old-<id> backup in the AddOns dir, the new ones are renamed in, and
// the backup is deleted. If placing fails, the backup is put back. Recover
// undoes a swap, and deletes temp files, that a crashed run left behind.
package install

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/maddawik/wam/internal/atomicfile"
	"github.com/maddawik/wam/internal/errs"
)

const (
	// oldPrefix names a backup holding the folders a swap replaced, in
	// asideDir, and, once they are all aside, a placingFile listing the
	// folders being put in their place.
	oldPrefix   = ".wam-old-"
	asideDir    = "d"
	placingFile = "placing"
	// tmpPrefix names anything that can be deleted outright after a crash.
	tmpPrefix = ".wam-tmp-"
	// TempPattern is the os.CreateTemp pattern for a temp file in the AddOns
	// dir that Recover deletes if a crash leaves it behind.
	TempPattern = tmpPrefix + "*"
)

// rename is os.Rename; tests make it fail.
var rename = os.Rename

// OwnerFunc returns the key of the other addon that owns folder, compared
// case-insensitively, or ok false when no other addon does.
type OwnerFunc func(folder string) (key string, ok bool)

// Zip extracts the zip at zipPath into a temp dir in addonsDir, then swaps
// its top-level folders in. It returns the folders, in the zip's casing. It
// never overwrites an existing folder except those named in owned, which an
// earlier install put there; those are replaced, and put back if the swap
// fails. ownerOf names the other addon that owns a folder, or returns ok
// false; a zip folder another addon owns, or one already in addonsDir that
// nothing owns, is a conflict. Folder names are compared case-insensitively.
// Every entry is checked before anything is written. Cancelling ctx stops
// the extraction; a swap, once started, finishes or is undone.
func Zip(ctx context.Context, zipPath, addonsDir string, owned []string, ownerOf OwnerFunc) (folders []string, err error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, errs.Wrap(errs.Runtime, err, "downloaded file is not a valid zip", "")
	}
	defer func() { _ = zr.Close() }()

	for _, f := range owned {
		if !isFolderName(f) {
			return nil, badFolder(f)
		}
	}
	folders, err = checkEntries(zr)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(addonsDir, 0o755); err != nil {
		return nil, errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot create %s", addonsDir), "")
	}
	if err := checkOwnership(folders, addonsDir, owned, ownerOf); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(addonsDir, tmpPrefix)
	if err != nil {
		return nil, errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot create a temp folder in %s", addonsDir), "")
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	if err := extract(ctx, zr, tmp); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := swap(addonsDir, tmp, owned, folders); err != nil {
		return nil, err
	}
	return folders, nil
}

// swap replaces the owned folders in addonsDir with folders from src.
func swap(addonsDir, src string, owned, folders []string) error {
	backup, err := os.MkdirTemp(addonsDir, oldPrefix)
	if err != nil {
		return errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot create a backup folder in %s", addonsDir), "")
	}
	aside := filepath.Join(backup, asideDir)
	if err := os.Mkdir(aside, 0o755); err != nil {
		_ = os.RemoveAll(backup)
		return errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot create a backup folder in %s", addonsDir), "")
	}
	// rollback undoes the swap and returns err. If undoing fails too,
	// Recover finishes it on the next run.
	rollback := func(err error) error {
		_ = undo(addonsDir, backup)
		return err
	}
	unfinished := func(err error) error {
		return rollback(errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot finish installing into %s", addonsDir), ""))
	}
	for _, f := range owned {
		err := rename(filepath.Join(addonsDir, f), filepath.Join(aside, f))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return rollback(folderError(err, f, addonsDir, "replace"))
		}
	}
	if err := atomicfile.Write(filepath.Join(backup, placingFile), []byte(strings.Join(folders, "\n"))); err != nil {
		return unfinished(err)
	}
	for _, f := range folders {
		if err := rename(filepath.Join(src, f), filepath.Join(addonsDir, f)); err != nil {
			return rollback(folderError(err, f, addonsDir, "place"))
		}
	}
	// Commit: renamed to a temp name, the backup is only ever deleted. Until
	// then, a crash means Recover undoes the swap.
	committed := filepath.Join(addonsDir, tmpPrefix+strings.TrimPrefix(filepath.Base(backup), oldPrefix))
	if err := rename(backup, committed); err != nil {
		return unfinished(err)
	}
	_ = os.RemoveAll(committed)
	return nil
}

// undo reverses an unfinished swap from its backup: it deletes the folders
// the swap placed, puts the replaced ones back, and deletes the backup. Each
// step can be repeated, so an undo that is itself interrupted can run again.
func undo(addonsDir, backup string) error {
	list, err := os.ReadFile(filepath.Join(backup, placingFile))
	switch {
	case err == nil:
		for _, f := range strings.Split(string(list), "\n") {
			if !isFolderName(f) {
				continue
			}
			if err := os.RemoveAll(filepath.Join(addonsDir, f)); err != nil {
				return folderError(err, f, addonsDir, "remove")
			}
		}
		// The replaced folders may share the placed ones' names; drop the
		// list before restoring them so a repeat doesn't delete them.
		if err := os.Remove(filepath.Join(backup, placingFile)); err != nil {
			return err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	aside := filepath.Join(backup, asideDir)
	es, err := os.ReadDir(aside)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, e := range es {
		if err := rename(filepath.Join(aside, e.Name()), filepath.Join(addonsDir, e.Name())); err != nil {
			return folderError(err, e.Name(), addonsDir, "restore")
		}
	}
	return os.RemoveAll(backup)
}

// Recover cleans up after a run that crashed or was killed: it undoes any
// unfinished swap, putting the replaced folders back, and deletes temp files.
func Recover(addonsDir string) error {
	es, err := os.ReadDir(addonsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot read %s", addonsDir), "")
	}
	for _, e := range es {
		p := filepath.Join(addonsDir, e.Name())
		switch {
		case strings.HasPrefix(e.Name(), oldPrefix):
			if err := undo(addonsDir, p); err != nil {
				if _, ok := errs.As(err); ok {
					return err
				}
				return errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot restore %s, left by an interrupted run", p), "")
			}
		case strings.HasPrefix(e.Name(), tmpPrefix):
			_ = os.RemoveAll(p)
		}
	}
	return nil
}

// Remove deletes folders from addonsDir. They are all moved aside first, so
// if one can't be, such as one in use, none is removed. A crash part way
// leaves some removed; the next sync removes the rest.
func Remove(addonsDir string, folders []string) error {
	for _, f := range folders {
		if !isFolderName(f) {
			return badFolder(f)
		}
	}
	tmp, err := os.MkdirTemp(addonsDir, tmpPrefix)
	if err != nil {
		return errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot create a temp folder in %s", addonsDir), "")
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	var moved []string
	for _, f := range folders {
		err := rename(filepath.Join(addonsDir, f), filepath.Join(tmp, f))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			for _, m := range moved {
				_ = rename(filepath.Join(tmp, m), filepath.Join(addonsDir, m))
			}
			return folderError(err, f, addonsDir, "remove")
		}
		moved = append(moved, f)
	}
	return nil
}

// folderError reports that folder couldn't be changed: a conflict asking to
// close WoW when it is in use, otherwise a runtime error. what is the verb
// for the change, such as "replace".
func folderError(err error, folder, addonsDir, what string) error {
	if inUse(err) {
		return &errs.Error{
			Kind: errs.Conflict, Err: err, Retryable: true,
			Message: fmt.Sprintf("%s in %s is in use or locked: close WoW and retry", folder, addonsDir),
		}
	}
	return errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot %s %s in %s", what, folder, addonsDir), "")
}

// isFolderName reports whether f names a folder directly in the AddOns dir.
func isFolderName(f string) bool {
	return filepath.IsLocal(f) && filepath.Base(f) == f
}

// badFolder is the error for a lock folder name that isFolderName refuses.
func badFolder(f string) error {
	return errs.New(errs.Conflict, fmt.Sprintf("lock lists folder %q, which is not a folder in the AddOns folder", f), "")
}

// checkOwnership refuses folders that another addon owns, or that are already
// in addonsDir unmanaged.
func checkOwnership(folders []string, addonsDir string, owned []string, ownerOf OwnerFunc) error {
	own := map[string]bool{}
	for _, f := range owned {
		own[strings.ToLower(f)] = true
	}
	present := map[string]bool{}
	entries, err := os.ReadDir(addonsDir)
	if err != nil {
		return errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot read %s", addonsDir), "")
	}
	for _, e := range entries {
		present[strings.ToLower(e.Name())] = true
	}
	for _, f := range folders {
		lf := strings.ToLower(f)
		if own[lf] {
			continue
		}
		if key, ok := ownerOf(f); ok {
			return errs.New(errs.Conflict, fmt.Sprintf("folder %s is owned by %s", f, key), "")
		}
		if present[lf] {
			return errs.New(errs.Conflict, fmt.Sprintf("folder %s is already in %s and is unmanaged", f, addonsDir), "")
		}
	}
	return nil
}

// entryName is a zip entry's path with "/" separators and no trailing slash.
// Some zips use "\" for separators.
func entryName(f *zip.File) string {
	return strings.TrimSuffix(strings.ReplaceAll(f.Name, `\`, "/"), "/")
}

// checkEntries refuses a zip with an entry that is not a regular file or folder
// inside an addon folder, and returns its top-level folders.
func checkEntries(zr *zip.ReadCloser) ([]string, error) {
	var folders []string
	seen := map[string]bool{}
	for _, f := range zr.File {
		name := entryName(f)
		if name == "" {
			continue
		}
		if path.IsAbs(name) || !filepath.IsLocal(filepath.FromSlash(name)) || strings.Contains(name, ":") {
			return nil, errs.New(errs.Conflict, fmt.Sprintf("zip entry %q escapes the AddOns folder", f.Name), "")
		}
		if mode := f.Mode(); !mode.IsDir() && !mode.IsRegular() {
			return nil, errs.New(errs.Conflict, fmt.Sprintf("zip entry %q is not a regular file", f.Name), "")
		}
		top, _, nested := strings.Cut(name, "/")
		if !nested && !f.FileInfo().IsDir() {
			return nil, errs.New(errs.Conflict, fmt.Sprintf("zip has a file %q outside any addon folder", f.Name), "")
		}
		if !seen[top] {
			seen[top] = true
			folders = append(folders, top)
		}
	}
	if len(folders) == 0 {
		return nil, errs.New(errs.Conflict, "zip contains no addon folders", "")
	}
	return folders, nil
}

// extract unpacks zr, which checkEntries accepted, into dir.
func extract(ctx context.Context, zr *zip.ReadCloser, dir string) error {
	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entryName(f)
		if name == "" {
			continue
		}
		if err := extractEntry(f, filepath.Join(dir, filepath.FromSlash(path.Clean(name)))); err != nil {
			return err
		}
	}
	return nil
}

func extractEntry(f *zip.File, dest string) error {
	if f.Mode().IsDir() {
		return os.MkdirAll(dest, 0o755)
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
