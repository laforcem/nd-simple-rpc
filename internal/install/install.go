// Package install copies the bundled Navidrome plugin into a plugins folder.
package install

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
)

// FileName is the plugin's file name inside the plugins folder.
const FileName = "nd-simple-rpc.ndp"

// Plugin writes src to dstDir/nd-simple-rpc.ndp through a temp file in dstDir
// and a rename, so Navidrome never sees a partial file. A non-empty connectURL
// replaces the manifest's website, which Navidrome's plugin page shows as a
// link. It does nothing and returns false when the destination already holds
// the same bytes.
func Plugin(src, dstDir, connectURL string) (changed bool, err error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return false, err
	}
	if connectURL != "" {
		if err := validURL(connectURL); err != nil {
			return false, err
		}
		if data, err = stampWebsite(data, connectURL); err != nil {
			return false, fmt.Errorf("stamp connect URL: %w", err)
		}
	}
	want := sha256.Sum256(data)
	dst := filepath.Join(dstDir, FileName)
	if have, err := sum(dst); err == nil && bytes.Equal(have, want[:]) {
		return false, nil
	}

	if err = os.MkdirAll(dstDir, 0o755); err != nil {
		return false, err
	}
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
	if _, err = tmp.Write(data); err != nil {
		return false, fmt.Errorf("write plugin: %w", err)
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

func validURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return errors.New("connect URL must be an absolute http(s) URL")
	}
	return nil
}

// stampWebsite rewrites manifest.json's website inside the .ndp zip. Entries
// keep their headers, so the same input always yields the same bytes and the
// unchanged-file check keeps working.
func stampWebsite(ndp []byte, website string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(ndp), int64(len(ndp)))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	found := false
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		if f.Name == "manifest.json" {
			found = true
			// RawMessage keeps every other field byte-for-byte.
			var m map[string]json.RawMessage
			if err := json.Unmarshal(body, &m); err != nil {
				return nil, fmt.Errorf("parse manifest: %w", err)
			}
			if m["website"], err = json.Marshal(website); err != nil {
				return nil, err
			}
			if body, err = json.MarshalIndent(m, "", "  "); err != nil {
				return nil, err
			}
		}
		h := f.FileHeader
		h.Extra = nil // CreateHeader writes its own timestamp extra
		w, err := zw.CreateHeader(&h)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(body); err != nil {
			return nil, err
		}
	}
	if err := zw.SetComment(zr.Comment); err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("manifest.json not found in plugin")
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
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
