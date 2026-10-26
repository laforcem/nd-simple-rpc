// Package install copies the bundled Navidrome plugin into a plugins folder.
package install

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// FileName is the plugin's file name inside the plugins folder.
const FileName = "nd-simple-rpc.ndp"

// Plugin copies src to dstDir/nd-simple-rpc.ndp through a temp file in dstDir
// and a rename, so Navidrome never sees a partial file. It does nothing and
// returns false when the destination already has the same sha256.
func Plugin(src, dstDir string) (changed bool, err error) {
	want, err := sum(src)
	if err != nil {
		return false, err
	}
	dst := filepath.Join(dstDir, FileName)
	if have, err := sum(dst); err == nil && bytes.Equal(have, want) {
		return false, nil
	}

	if err = os.MkdirAll(dstDir, 0o755); err != nil {
		return false, err
	}
	in, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(dstDir, FileName+".tmp-*")
	if err != nil {
		return false, err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err = io.Copy(tmp, in); err != nil {
		return false, fmt.Errorf("copy plugin: %w", err)
	}
	if err = tmp.Chmod(0o644); err != nil {
		return false, err
	}
	if err = tmp.Close(); err != nil {
		return false, err
	}
	if err = os.Rename(tmp.Name(), dst); err != nil {
		return false, err
	}
	return true, nil
}

func sum(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
