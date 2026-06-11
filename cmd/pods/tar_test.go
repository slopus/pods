package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"testing"
)

func TestPackageDirSkipsIgnoredEntries(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "index.html"), "hello")
	writeFile(t, filepath.Join(dir, "pods.json"), `{"name":"skip"}`)
	writeFile(t, filepath.Join(dir, ".env"), "skip")
	writeFile(t, filepath.Join(dir, ".git", "config"), "skip")
	writeFile(t, filepath.Join(dir, "node_modules", "dep.js"), "skip")
	writeFile(t, filepath.Join(dir, "assets", "app.js"), "console.log('ok')")

	var buf testBuffer
	files, total, err := packageDir(&buf, dir)
	if err != nil {
		t.Fatalf("packageDir: %v", err)
	}
	if files != 2 {
		t.Fatalf("files = %d, want 2", files)
	}
	if total == 0 {
		t.Fatal("total = 0, want non-zero")
	}

	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gz.Close()

	var names []string
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		names = append(names, hdr.Name)
	}
	slices.Sort(names)
	want := []string{"assets/app.js", "index.html"}
	if !slices.Equal(names, want) {
		t.Fatalf("tar entries = %v, want %v", names, want)
	}
}

func TestResolveConfigPrecedence(t *testing.T) {
	file := config{Endpoint: "http://file", Secret: "file-secret"}
	env := func(key string) string {
		switch key {
		case "PODS_ENDPOINT":
			return "http://env"
		case "PODS_SECRET":
			return "env-secret"
		default:
			return ""
		}
	}

	cfg := resolveConfig("http://flag", "flag-secret", env, file)
	if cfg.Endpoint != "http://flag" || cfg.Secret != "flag-secret" {
		t.Fatalf("flags did not win: %+v", cfg)
	}

	cfg = resolveConfig("", "", env, file)
	if cfg.Endpoint != "http://env" || cfg.Secret != "env-secret" {
		t.Fatalf("env did not beat file: %+v", cfg)
	}

	cfg = resolveConfig("", "", func(string) string { return "" }, file)
	if cfg.Endpoint != "http://file" || cfg.Secret != "file-secret" {
		t.Fatalf("file config not used: %+v", cfg)
	}
}
