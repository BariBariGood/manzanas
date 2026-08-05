package state

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// maxImageUnpackBytes caps the total decompressed size of one archive
// extraction, so a tampered or foreign archive (they are plain files,
// copyable between hosts) cannot fill the host disk. Sim data dirs are a
// few GB; 64 GiB is far beyond any real image.
const maxImageUnpackBytes = 64 << 30

// Golden-image archive format: a zstd-compressed tar containing the
// device data directory under "data/" plus the device's "device.plist"
// (when present) for provenance. See docs/images.md.

// packImage writes dataDir (as "data/...") and plistPath (as
// "device.plist", skipped if missing) into a tar.zst at archive,
// returning the archive size in bytes. The write is staged into a .tmp
// sibling and renamed in, so a failed pack never leaves a partial archive.
func packImage(ctx context.Context, archive, dataDir, plistPath string) (int64, error) {
	tmp := archive + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp)
	zw, err := zstd.NewWriter(f)
	if err != nil {
		f.Close()
		return 0, err
	}
	tw := tar.NewWriter(zw)

	if err := tarTree(ctx, tw, dataDir, "data"); err != nil {
		tw.Close()
		zw.Close()
		f.Close()
		return 0, err
	}
	if _, err := os.Stat(plistPath); err == nil {
		if err := tarFile(tw, plistPath, "device.plist"); err != nil {
			tw.Close()
			zw.Close()
			f.Close()
			return 0, err
		}
	}
	if err := tw.Close(); err != nil {
		zw.Close()
		f.Close()
		return 0, err
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, archive); err != nil {
		return 0, err
	}
	st, err := os.Stat(archive)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// tarTree walks root and writes every entry under prefix/ (dirs, regular
// files, and symlinks; sockets and other specials are skipped — sims hold
// unix sockets that cannot and need not be archived).
func tarTree(ctx context.Context, tw *tar.Writer, root, prefix string) error {
	return filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			// System-protected sim cache files (e.g. locationd's
			// locScoreInfo) deny reads even to the owner; they aren't
			// needed for a deterministic image, so skip them.
			if os.IsPermission(err) {
				if fi != nil && fi.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name := prefix
		if rel != "." {
			name = prefix + "/" + filepath.ToSlash(rel)
		}
		switch {
		case fi.IsDir():
			hdr, err := tar.FileInfoHeader(fi, "")
			if err != nil {
				return err
			}
			hdr.Name = name + "/"
			return tw.WriteHeader(hdr)
		case fi.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			// Only archive symlinks the extractor will accept: targets
			// that stay inside the archived tree. Out-of-tree links (e.g.
			// absolute paths into the runtime bundle) are host-specific
			// and skipped, keeping pack and unpack symmetric.
			if checkLinkTarget(name, link) != nil {
				return nil
			}
			hdr, err := tar.FileInfoHeader(fi, link)
			if err != nil {
				return err
			}
			hdr.Name = name
			return tw.WriteHeader(hdr)
		case fi.Mode().IsRegular():
			// Open before writing the header, so an unreadable
			// (permission-denied) file is skipped cleanly instead of
			// leaving a truncated tar entry.
			src, err := os.Open(path)
			if err != nil {
				if os.IsPermission(err) {
					return nil
				}
				return err
			}
			hdr, err := tar.FileInfoHeader(fi, "")
			if err != nil {
				src.Close()
				return err
			}
			hdr.Name = name
			if err := tw.WriteHeader(hdr); err != nil {
				src.Close()
				return err
			}
			_, err = io.Copy(tw, src)
			src.Close()
			return err
		default:
			return nil // sockets, pipes, devices: skip
		}
	})
}

// tarFile writes one regular file into the tar under name.
func tarFile(tw *tar.Writer, path, name string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	hdr, err := tar.FileInfoHeader(fi, "")
	if err != nil {
		return err
	}
	hdr.Name = name
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	_, err = io.Copy(tw, src)
	return err
}

// unpackImage extracts an image archive into destDir (the "data" tree
// lands at destDir/data; "device.plist" is skipped — a stamped sim keeps
// its own identity). Entry names are validated so a tampered archive can
// never write outside destDir.
func unpackImage(ctx context.Context, archive, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := zstd.NewReader(f, zstd.WithDecoderMaxMemory(1<<30))
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	remaining := int64(maxImageUnpackBytes)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if name == "device.plist" {
			continue
		}
		// The Clean above collapses any ".." components, so this prefix
		// check is the containment guarantee (and legitimate filenames
		// that merely contain dots are fine).
		if name != "data" && !strings.HasPrefix(name, "data"+string(os.PathSeparator)) {
			return fmt.Errorf("image archive: unexpected entry %q", hdr.Name)
		}
		dest := filepath.Join(destDir, name)
		mode := os.FileMode(hdr.Mode) & os.ModePerm
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := resolvedWithin(destDir, dest); err != nil {
				return err
			}
			if err := os.MkdirAll(dest, mode|0o700); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// A link target must stay inside the extracted "data" tree, or
			// later writes through the link could escape destDir.
			if err := checkLinkTarget(name, hdr.Linkname); err != nil {
				return err
			}
			if err := resolvedWithin(destDir, dest); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return err
			}
			// Re-validate the target against the link's *real* location:
			// an earlier in-tree symlink in the entry's path can make the
			// lexical name and the actual placement diverge, and only the
			// latter matters for containment.
			realParent, err := filepath.EvalSymlinks(filepath.Dir(dest))
			if err != nil {
				return err
			}
			rroot, err := filepath.EvalSymlinks(destDir)
			if err != nil {
				return err
			}
			relParent, err := filepath.Rel(rroot, realParent)
			if err != nil {
				return err
			}
			if err := checkLinkTarget(filepath.Join(relParent, filepath.Base(dest)), hdr.Linkname); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, dest); err != nil && !os.IsExist(err) {
				return err
			}
		case tar.TypeReg:
			if err := resolvedWithin(destDir, dest); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			n, err := io.Copy(out, io.LimitReader(tr, remaining+1))
			remaining -= n
			if err != nil {
				out.Close()
				return err
			}
			if remaining < 0 {
				out.Close()
				return fmt.Errorf("image archive: decompressed size exceeds %d bytes", maxImageUnpackBytes)
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			// skip other entry types
		}
	}
}

// resolvedWithin guards against symlink chains: the lexical checks on
// entry names and link targets can be defeated by routing a path through
// symlinks created earlier in the same extraction, so before anything is
// created at dest, the deepest already-existing ancestor of dest (which
// resolves every existing symlink on the way) must land inside root.
func resolvedWithin(root, dest string) error {
	rroot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	p := dest
	for {
		if _, err := os.Lstat(p); err == nil {
			break
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		return fmt.Errorf("image archive: entry %q resolves outside extraction dir: %w", dest, err)
	}
	if rp != rroot && !strings.HasPrefix(rp, rroot+string(os.PathSeparator)) {
		return fmt.Errorf("image archive: entry %q escapes extraction dir via symlink", dest)
	}
	return nil
}

// checkLinkTarget rejects a symlink entry whose target resolves outside
// the extracted "data" tree (absolute targets, or enough ".." to escape).
func checkLinkTarget(entryName, linkname string) error {
	if filepath.IsAbs(linkname) {
		return fmt.Errorf("image archive: absolute symlink target in entry %q", entryName)
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(entryName), filepath.FromSlash(linkname)))
	if resolved != "data" && !strings.HasPrefix(resolved, "data"+string(os.PathSeparator)) {
		return fmt.Errorf("image archive: symlink target escapes archive in entry %q", entryName)
	}
	return nil
}
