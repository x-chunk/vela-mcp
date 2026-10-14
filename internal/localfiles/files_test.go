package localfiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContainmentAndDownloadCleanup(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	f, err := New([]string{root}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, p := range []string{secret, filepath.Join(root, "escape"), root, "relative.txt", filepath.Join(root, "..", "outside")} {
		if file, _, err := f.OpenUpload(p, 100); err == nil {
			file.Close()
			t.Fatalf("accepted path outside capability: %s", p)
		}
	}
	data := "hello"
	digest := sha256.Sum256([]byte(data))
	sha := hex.EncodeToString(digest[:])
	target := filepath.Join(root, "download")
	if n, err := f.SaveDownload(context.Background(), target, strings.NewReader(data), 100, sha); err != nil || n != 5 {
		t.Fatalf("download failed: %v", err)
	}
	if _, err := f.SaveDownload(context.Background(), target, strings.NewReader("overwrite"), 100, sha); err == nil {
		t.Fatal("overwrote existing file")
	}
	file, n, err := f.OpenUpload(target, 100)
	if err != nil || n != 5 {
		t.Fatal("regular source rejected")
	}
	file.Close()
	if file, _, err := f.OpenUpload(target, 4); err == nil {
		file.Close()
		t.Fatal("oversized source accepted")
	}
	for _, tc := range []struct {
		name   string
		max    int64
		sha    string
		cancel bool
	}{{"badsha", 100, "bad", false}, {"overflow", 3, sha, false}, {"cancelled", 100, sha, true}} {
		ctx, cancel := context.WithCancel(context.Background())
		if tc.cancel {
			cancel()
		}
		p := filepath.Join(root, tc.name)
		if _, err := f.SaveDownload(ctx, p, strings.NewReader(data), tc.max, tc.sha); err == nil {
			t.Fatal("incomplete download succeeded")
		}
		cancel()
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatal("incomplete output was left on disk")
		}
	}
}
