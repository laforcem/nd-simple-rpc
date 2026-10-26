package install

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInstallFresh(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.ndp")
	write(t, src, "plugin-v1")
	dst := t.TempDir()
	changed, err := Plugin(src, dst)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "nd-simple-rpc.ndp"))
	if err != nil || string(got) != "plugin-v1" {
		t.Fatalf("got %q err=%v", got, err)
	}
}

func TestInstallIdenticalNoop(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.ndp")
	write(t, src, "plugin-v1")
	dst := t.TempDir()
	if _, err := Plugin(src, dst); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dst, "nd-simple-rpc.ndp")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(target, old, old); err != nil {
		t.Fatal(err)
	}
	changed, err := Plugin(src, dst)
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	fi, _ := os.Stat(target)
	if !fi.ModTime().Equal(old) {
		t.Fatalf("mtime changed: %v", fi.ModTime())
	}
}

func TestInstallReplaces(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.ndp")
	dst := t.TempDir()
	write(t, src, "plugin-v1")
	if _, err := Plugin(src, dst); err != nil {
		t.Fatal(err)
	}
	write(t, src, "plugin-v2")
	changed, err := Plugin(src, dst)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(filepath.Join(dst, "nd-simple-rpc.ndp"))
	if string(got) != "plugin-v2" {
		t.Fatalf("got %q", got)
	}
	ents, _ := os.ReadDir(dst)
	if len(ents) != 1 {
		t.Fatalf("want only the .ndp, got %d entries", len(ents))
	}
}

func TestInstallNoPartialFileOnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	src := filepath.Join(t.TempDir(), "src.ndp")
	write(t, src, "plugin-v1")
	dst := t.TempDir()
	if err := os.Chmod(dst, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dst, 0o755) })
	if _, err := Plugin(src, dst); err == nil {
		t.Fatal("want error")
	}
	ents, _ := os.ReadDir(dst)
	if len(ents) != 0 {
		t.Fatalf("leftover files: %v", ents)
	}
}
