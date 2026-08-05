package state

import (
	"context"
	"io"
	"os"
	"path/filepath"
)

// copyTreeTolerant copies the tree at src to dst (which must not exist),
// skipping entries the process is not permitted to read. Booted sims hold
// system-protected cache files (e.g. locationd's locScoreInfo) that fail
// reads with EPERM even for the owner; they are not needed for a
// deterministic restore, so a permission-denied entry is skipped rather
// than failing the whole copy. Returns the number of skipped entries.
func copyTreeTolerant(ctx context.Context, src, dst string) (int, error) {
	skipped := 0
	err := filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			if os.IsPermission(err) {
				skipped++
				if fi != nil && fi.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(dst, rel)
		mode := fi.Mode()
		switch {
		case mode.IsDir():
			// Ensure the copy is traversable/writable by the daemon even
			// when the source dir is more restrictive.
			return os.MkdirAll(dest, mode.Perm()|0o700)
		case mode&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				if os.IsPermission(err) {
					skipped++
					return nil
				}
				return err
			}
			return os.Symlink(link, dest)
		case mode.IsRegular():
			in, err := os.Open(path)
			if err != nil {
				if os.IsPermission(err) {
					skipped++
					return nil
				}
				return err
			}
			defer in.Close()
			out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm()|0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				out.Close()
				if os.IsPermission(err) {
					skipped++
					_ = os.Remove(dest)
					return nil
				}
				return err
			}
			return out.Close()
		default:
			return nil // sockets, pipes, devices: not needed for restore
		}
	})
	return skipped, err
}
