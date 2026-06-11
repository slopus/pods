package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// packageDir writes a tar.gz archive of dir to w, skipping .git,
// node_modules, pods.json and all dotfiles. Only regular files are
// archived (symlinks and other special files are silently skipped; the
// server rejects them anyway). It returns the number of files added and
// their total uncompressed size in bytes.
func packageDir(w io.Writer, dir string) (files int, total int64, err error) {
	info, err := os.Stat(dir)
	if err != nil {
		return 0, 0, err
	}
	if !info.IsDir() {
		return 0, 0, fmt.Errorf("%s is not a directory", dir)
	}

	gw := gzip.NewWriter(w)
	tw := tar.NewWriter(gw)

	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if skipEntry(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:     filepath.ToSlash(rel),
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     fi.Size(),
			ModTime:  fi.ModTime(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		n, err := io.Copy(tw, f)
		f.Close()
		if err != nil {
			return err
		}
		files++
		total += n
		return nil
	})
	if walkErr != nil {
		return 0, 0, walkErr
	}
	if err := tw.Close(); err != nil {
		return 0, 0, err
	}
	if err := gw.Close(); err != nil {
		return 0, 0, err
	}
	return files, total, nil
}

// skipEntry reports whether a file or directory name is excluded from
// deploy packages: dotfiles (which covers .git), node_modules and pods.json.
func skipEntry(name string) bool {
	return strings.HasPrefix(name, ".") || name == "node_modules" || name == "pods.json"
}
