package webassets

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// keepFile is the checked-in placeholder that lets dist be embedded before
// a build exists.
const keepFile = ".gitkeep"

// Sync replaces everything in dst except the placeholder with the complete
// build in src. It refuses a build without index.html before touching dst.
func Sync(src, dst string) error {
	if info, err := os.Stat(filepath.Join(src, "index.html")); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("sync web assets: %s has no index.html; run npm run build", src)
	}
	entries, err := os.ReadDir(dst)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("sync web assets: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == keepFile {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dst, entry.Name())); err != nil {
			return fmt.Errorf("sync web assets: remove stale %s: %w", entry.Name(), err)
		}
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type().IsRegular():
			return copyFile(path, target)
		default:
			return fmt.Errorf("sync web assets: %s is not a regular file", rel)
		}
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
