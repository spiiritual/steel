// Package update installs signed steel releases from GitHub.
//
// Every release carries checksums.txt (SHA-256 of each platform's binary)
// and checksums.txt.minisig, a minisign signature by the release key whose
// public half is built in below. Nothing is installed unless the signature
// verifies, its trusted comment names the release's own version (so an old
// signed release can't be republished under a newer tag), and the
// downloaded binary matches its checksum.
package update

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"aead.dev/minisign"
	"golang.org/x/mod/semver"
)

// ReleaseKey is the minisign public key releases are signed with.
const ReleaseKey = "RWQAItnwB4Wl279LXjvqxB/ZFZpsyrDkMGBbjZJp9bKhIuw+IiXAsxLR"

// Download size caps, so a hostile server can't fill the disk.
const (
	maxBinary   = 256 << 20
	maxMetadata = 1 << 20
)

// Updater finds and installs releases.
type Updater struct {
	API      string // GitHub API base URL
	Repo     string // owner/name
	Key      minisign.PublicKey
	HTTP     *http.Client
	OS, Arch string // platform to install for
}

// New returns an Updater for steel's GitHub releases and this platform.
func New() *Updater {
	u := &Updater{API: "https://api.github.com", Repo: "spiiritual/steel", HTTP: http.DefaultClient, OS: runtime.GOOS, Arch: runtime.GOARCH}
	if err := u.Key.UnmarshalText([]byte(ReleaseKey)); err != nil {
		panic("update: bad ReleaseKey: " + err.Error())
	}
	return u
}

// Release is a published version and its downloadable assets.
type Release struct {
	Version string
	assets  map[string]string // name → download URL
}

// Latest returns the newest published release.
func (u *Updater) Latest(ctx context.Context) (Release, error) {
	var body struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	b, err := u.get(ctx, u.API+"/repos/"+u.Repo+"/releases/latest", maxMetadata)
	if errors.Is(err, errNotFound) {
		return Release{}, fmt.Errorf("%s has no published releases yet", u.Repo)
	}
	if err != nil {
		return Release{}, err
	}
	if err := json.Unmarshal(b, &body); err != nil {
		return Release{}, fmt.Errorf("latest release: %w", err)
	}
	if !semver.IsValid(body.TagName) {
		return Release{}, fmt.Errorf("latest release has no version tag (%q)", body.TagName)
	}
	r := Release{Version: body.TagName, assets: map[string]string{}}
	for _, a := range body.Assets {
		r.assets[a.Name] = a.URL
	}
	return r, nil
}

// Newer reports whether latest is newer than current. A current version
// that isn't semver (e.g. "unknown") counts as older. Development builds
// have pseudo-versions like v0.0.0-20261117…-abc, older than any release.
func Newer(current, latest string) bool {
	if !semver.IsValid(current) {
		return true
	}
	return semver.Compare(latest, current) > 0
}

// Install downloads r's binary for this platform, verifies it, and
// atomically replaces the executable at exe with it. The replaced binary
// is kept as exe+".prev" for Rollback.
func (u *Updater) Install(ctx context.Context, r Release, exe string) error {
	name := fmt.Sprintf("steel-%s-%s", u.OS, u.Arch)
	sums, err := u.verifiedChecksums(ctx, r)
	if err != nil {
		return err
	}
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("%s has no build for %s/%s", r.Version, u.OS, u.Arch)
	}
	url, ok := r.assets[name]
	if !ok {
		return fmt.Errorf("%s is missing %s", r.Version, name)
	}

	// Download next to exe, so the final rename stays on one filesystem.
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".steel-update-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed into place
	h := sha256.New()
	err = u.download(ctx, url, maxBinary, io.MultiWriter(tmp, h))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s: checksum mismatch (got %s, want %s)", name, got, want)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	if err := keepPrevious(exe); err != nil {
		return fmt.Errorf("keeping the current binary: %w", err)
	}
	return os.Rename(tmp.Name(), exe)
}

// verifiedChecksums downloads and verifies r's signed checksums.
func (u *Updater) verifiedChecksums(ctx context.Context, r Release) (map[string]string, error) {
	sumsURL, ok1 := r.assets["checksums.txt"]
	sigURL, ok2 := r.assets["checksums.txt.minisig"]
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("%s is missing checksums.txt or its signature", r.Version)
	}
	sums, err := u.get(ctx, sumsURL, maxMetadata)
	if err != nil {
		return nil, err
	}
	sigText, err := u.get(ctx, sigURL, maxMetadata)
	if err != nil {
		return nil, err
	}
	if !minisign.Verify(u.Key, sums, sigText) {
		return nil, fmt.Errorf("%s: checksums.txt is not signed by steel's release key", r.Version)
	}
	var sig minisign.Signature
	if err := sig.UnmarshalText(sigText); err != nil {
		return nil, err
	}
	if sig.TrustedComment != "steel "+r.Version {
		return nil, fmt.Errorf("%s: signature is for %q, not this release", r.Version, sig.TrustedComment)
	}
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		if sum, file, ok := strings.Cut(sc.Text(), "  "); ok {
			out[file] = sum
		}
	}
	return out, sc.Err()
}

// keepPrevious saves exe as exe+".prev", replacing an older one. A hard
// link keeps exe in place until the new binary is renamed over it.
func keepPrevious(exe string) error {
	prev := exe + ".prev"
	if err := os.Remove(prev); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Link(exe, prev); err == nil {
		return nil
	}
	return copyFile(exe, prev)
}

// Rollback swaps the executable at exe with the copy Install kept, so
// running it again rolls forward.
func Rollback(exe string) error {
	prev := exe + ".prev"
	if _, err := os.Stat(prev); err != nil {
		return fmt.Errorf("no previous version to roll back to (%w)", err)
	}
	cur := exe + ".cur"
	if err := os.Remove(cur); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Link(exe, cur); err != nil {
		if err := copyFile(exe, cur); err != nil {
			return err
		}
	}
	if err := os.Rename(prev, exe); err != nil {
		return err
	}
	return os.Rename(cur, prev)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	var b bytes.Buffer
	if err := u.download(ctx, url, limit, &b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// download writes url's body to w, failing if it exceeds limit bytes.
func (u *Updater) download(ctx context.Context, url string, limit int64, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "steel-updater")
	req.Header.Set("Accept", "application/vnd.github+json, application/octet-stream")
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("GET %s: %w", url, errNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("GET %s: larger than %d bytes", url, limit)
	}
	return nil
}

var errNotFound = errors.New("404 Not Found")
