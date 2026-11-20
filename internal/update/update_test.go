package update

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aead.dev/minisign"
)

// fakeRelease serves a release the way GitHub does: the latest-release API
// and asset downloads.
type fakeRelease struct {
	tag    string
	files  map[string][]byte // asset name → contents
	server *httptest.Server
}

func newFakeRelease(t *testing.T, priv minisign.PrivateKey, tag, comment string, binary []byte) *fakeRelease {
	t.Helper()
	sum := sha256.Sum256(binary)
	sums := fmt.Sprintf("%s  steel-linux-amd64\n", hex.EncodeToString(sum[:]))
	f := &fakeRelease{tag: tag, files: map[string][]byte{
		"steel-linux-amd64":     binary,
		"checksums.txt":         []byte(sums),
		"checksums.txt.minisig": minisign.SignWithComments(priv, []byte(sums), comment, ""),
	}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/steel/releases/latest" {
			var assets []map[string]string
			for name := range f.files {
				assets = append(assets, map[string]string{"name": name, "browser_download_url": f.server.URL + "/dl/" + name})
			}
			json.NewEncoder(w).Encode(map[string]any{"tag_name": f.tag, "assets": assets})
			return
		}
		if b, ok := f.files[strings.TrimPrefix(r.URL.Path, "/dl/")]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func setup(t *testing.T) (minisign.PrivateKey, *Updater, string) {
	t.Helper()
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "steel")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return priv, &Updater{Repo: "o/steel", Key: pub, HTTP: http.DefaultClient, OS: "linux", Arch: "amd64"}, exe
}

func install(t *testing.T, u *Updater, f *fakeRelease, exe string) error {
	t.Helper()
	u.API = f.server.URL
	r, err := u.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return u.Install(context.Background(), r, exe)
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}

func TestInstallAndRollback(t *testing.T) {
	priv, u, exe := setup(t)
	f := newFakeRelease(t, priv, "v1.2.0", "steel v1.2.0", []byte("new binary"))
	if err := install(t, u, f, exe); err != nil {
		t.Fatal(err)
	}
	if read(t, exe) != "new binary" || read(t, exe+".prev") != "old binary" {
		t.Fatalf("after install: exe %q, prev %q", read(t, exe), read(t, exe+".prev"))
	}
	if fi, _ := os.Stat(exe); fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("installed binary not executable: %v", fi.Mode())
	}
	// Rollback swaps, so a second one rolls forward again.
	if err := Rollback(exe); err != nil || read(t, exe) != "old binary" || read(t, exe+".prev") != "new binary" {
		t.Fatalf("rollback: %v, exe %q, prev %q", err, read(t, exe), read(t, exe+".prev"))
	}
	if err := Rollback(exe); err != nil || read(t, exe) != "new binary" {
		t.Fatalf("roll forward: %v, exe %q", err, read(t, exe))
	}
	if entries, _ := os.ReadDir(filepath.Dir(exe)); len(entries) != 2 {
		t.Errorf("leftover files: %d entries", len(entries))
	}
}

func TestInstallRefusesUnverifiedReleases(t *testing.T) {
	for name, tamper := range map[string]func(f *fakeRelease, other minisign.PrivateKey){
		"binary changed after signing": func(f *fakeRelease, _ minisign.PrivateKey) {
			f.files["steel-linux-amd64"] = []byte("evil binary")
		},
		"signed by another key": func(f *fakeRelease, other minisign.PrivateKey) {
			f.files["checksums.txt.minisig"] = minisign.SignWithComments(other, f.files["checksums.txt"], "steel v1.2.0", "")
		},
		"old release under a new tag": func(f *fakeRelease, _ minisign.PrivateKey) {
			f.tag = "v9.0.0" // assets and signature still say v1.2.0
		},
		"no signature": func(f *fakeRelease, _ minisign.PrivateKey) {
			delete(f.files, "checksums.txt.minisig")
		},
	} {
		t.Run(name, func(t *testing.T) {
			priv, u, exe := setup(t)
			_, other, _ := minisign.GenerateKey(rand.Reader)
			f := newFakeRelease(t, priv, "v1.2.0", "steel v1.2.0", []byte("new binary"))
			tamper(f, other)
			if err := install(t, u, f, exe); err == nil {
				t.Fatal("installed an unverified release")
			}
			if read(t, exe) != "old binary" {
				t.Errorf("exe changed to %q", read(t, exe))
			}
			if entries, _ := os.ReadDir(filepath.Dir(exe)); len(entries) != 1 {
				t.Errorf("leftover files: %d entries", len(entries))
			}
		})
	}
}

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		current, latest string
		want            bool
	}{
		{"v0.1.0", "v0.2.0", true},
		{"v0.2.0", "v0.2.0", false},
		{"v0.3.0", "v0.2.0", false},
		{"v0.0.0-20261117031458-184b211619f7+dirty", "v0.1.0", true}, // dev build
		{"unknown", "v0.1.0", true},
	} {
		if got := Newer(tc.current, tc.latest); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v", tc.current, tc.latest, got)
		}
	}
}

func TestImageDigest(t *testing.T) {
	priv, u, _ := setup(t)
	f := newFakeRelease(t, priv, "v1.2.0", "steel v1.2.0", []byte("new binary"))
	digest := strings.Repeat("ab", sha256.Size)
	resign := func(sums string) {
		f.files["checksums.txt"] = []byte(sums)
		f.files["checksums.txt.minisig"] = minisign.SignWithComments(priv, []byte(sums), "steel v1.2.0", "")
	}
	latest := func() (string, error) {
		u.API = f.server.URL
		r, err := u.Latest(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return u.ImageDigest(context.Background(), r)
	}

	if _, err := latest(); err == nil {
		t.Error("a release without an image line gave a digest")
	}
	base := string(f.files["checksums.txt"])
	resign(base + digest + "  image\n")
	if got, err := latest(); err != nil || got != "sha256:"+digest {
		t.Errorf("ImageDigest = %q, %v", got, err)
	}
	// The digest is only as good as the signature over it.
	f.files["checksums.txt"] = []byte(base + strings.Repeat("cd", sha256.Size) + "  image\n")
	if got, err := latest(); err == nil {
		t.Errorf("unsigned digest accepted: %q", got)
	}
	resign(base + "not-hex  image\n")
	if got, err := latest(); err == nil {
		t.Errorf("malformed digest accepted: %q", got)
	}
}
