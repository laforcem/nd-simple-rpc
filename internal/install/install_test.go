package install

import (
	"archive/zip"
	"encoding/json"
	"io"
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
	changed, err := Plugin(src, dst, "")
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
	if _, err := Plugin(src, dst, ""); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dst, "nd-simple-rpc.ndp")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(target, old, old); err != nil {
		t.Fatal(err)
	}
	changed, err := Plugin(src, dst, "")
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
	if _, err := Plugin(src, dst, ""); err != nil {
		t.Fatal(err)
	}
	write(t, src, "plugin-v2")
	changed, err := Plugin(src, dst, "")
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
	if _, err := Plugin(src, dst, ""); err == nil {
		t.Fatal("want error")
	}
	ents, _ := os.ReadDir(dst)
	if len(ents) != 0 {
		t.Fatalf("leftover files: %v", ents)
	}
}

func writeNDP(t *testing.T, path string, manifest map[string]any) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	m, _ := json.Marshal(manifest)
	for name, body := range map[string][]byte{"plugin.wasm": []byte("\x00asm-bytes"), "manifest.json": m} {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
		if err != nil {
			t.Fatal(err)
		}
		w.Write(body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func readNDP(t *testing.T, path string) (manifest map[string]any, wasm string) {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		switch f.Name {
		case "manifest.json":
			if err := json.Unmarshal(b, &manifest); err != nil {
				t.Fatal(err)
			}
		case "plugin.wasm":
			wasm = string(b)
		}
	}
	return manifest, wasm
}

func TestInstallStampsConnectURL(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.ndp")
	writeNDP(t, src, map[string]any{"name": "nd-simple-rpc", "version": "0.1.1", "website": "https://github.com/laforcem/nd-simple-rpc"})
	dst := t.TempDir()
	changed, err := Plugin(src, dst, "https://presence.example.com")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	m, wasm := readNDP(t, filepath.Join(dst, FileName))
	if m["website"] != "https://presence.example.com" {
		t.Errorf("website = %v", m["website"])
	}
	if m["name"] != "nd-simple-rpc" || m["version"] != "0.1.1" {
		t.Errorf("other manifest fields not preserved: %v", m)
	}
	if wasm != "\x00asm-bytes" {
		t.Errorf("plugin.wasm changed: %q", wasm)
	}
}

func TestInstallStampedIdempotent(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.ndp")
	writeNDP(t, src, map[string]any{"name": "nd-simple-rpc", "website": "https://github.com/x"})
	dst := t.TempDir()
	if _, err := Plugin(src, dst, "https://presence.example.com"); err != nil {
		t.Fatal(err)
	}
	changed, err := Plugin(src, dst, "https://presence.example.com")
	if err != nil || changed {
		t.Fatalf("second run changed=%v err=%v", changed, err)
	}
	changed, err = Plugin(src, dst, "https://other.example.com")
	if err != nil || !changed {
		t.Fatalf("new URL changed=%v err=%v", changed, err)
	}
}

func TestInstallRejectsBadConnectURL(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.ndp")
	writeNDP(t, src, map[string]any{"name": "nd-simple-rpc"})
	for _, u := range []string{"presence.example.com", "ftp://x.example.com", "https://", "javascript:alert(1)"} {
		if _, err := Plugin(src, t.TempDir(), u); err == nil {
			t.Errorf("%q: want error", u)
		}
	}
}
