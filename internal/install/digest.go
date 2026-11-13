package install

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/maddawik/wam/internal/errs"
)

// Digest returns a SHA-256 over the names and contents of every file under
// folders in addonsDir. A folder that is absent contributes nothing, so a
// removed or emptied addon digests differently from an installed one.
// Names are hashed with forward slashes, so the digest is the same on every OS.
func Digest(addonsDir string, folders []string) (string, error) {
	h := sha256.New()
	for _, folder := range slices.Sorted(slices.Values(folders)) {
		root := filepath.Join(addonsDir, folder)
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(addonsDir, p)
			if err != nil {
				return err
			}
			name := filepath.ToSlash(rel)
			if d.IsDir() {
				_, err = fmt.Fprintf(h, "d %s\n", name)
				return err
			}
			if !d.Type().IsRegular() {
				_, err = fmt.Fprintf(h, "o %s\n", name)
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			fh := sha256.New()
			if _, err := io.Copy(fh, f); err != nil {
				return err
			}
			_, err = fmt.Fprintf(h, "f %s %x\n", name, fh.Sum(nil))
			return err
		})
		if err != nil && !os.IsNotExist(err) {
			return "", errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot read %s", strings.TrimSpace(root)), "")
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
