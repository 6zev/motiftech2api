// Package update implements self-update from GitHub releases.
//
// Safety model, in order, so a failed update can never leave a broken binary:
//
//  1. download to a temporary file next to the target (same filesystem, so the
//     final rename is atomic)
//  2. verify the SHA-256 against the release's checksums.txt; refuse if the
//     archive carries no entry for this asset
//  3. run the downloaded file with --version and require it to report the
//     expected version; refuse otherwise
//  4. only then os.Rename over the running binary, which is atomic on POSIX and
//     keeps the original permissions
//
// If any step fails the temporary file is removed and the original is untouched.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultRepo is where releases are published.
const DefaultRepo = "6zev/motiftech2api"

// DefaultAPIBase is the GitHub API root. Override with APIBase for a mirror or
// GitHub Enterprise.
const DefaultAPIBase = "https://api.github.com"

// Release is the subset of the GitHub API we need.
type Release struct {
	TagName string  `json:"tag_name"`
	Name    string  `json:"name"`
	Assets  []Asset `json:"assets"`
	Draft   bool    `json:"draft"`
	Pre     bool    `json:"prerelease"`
}

// Asset is one release file.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Options configure an update check.
type Options struct {
	Repo       string
	APIBase    string // defaults to DefaultAPIBase
	CurrentVer string
	HTTP       *http.Client
	GOOS       string
	GOARCH     string
	Target     string // path to the running binary
	AllowDraft bool
	AllowPre   bool
	Token      string // optional, for a private fork or higher rate limits
	Out        io.Writer
	// CheckOnly stops after resolving whether a newer release exists, without
	// downloading or touching the target.
	CheckOnly bool
}

// Result describes what happened.
type Result struct {
	Updated bool
	From    string
	To      string
	Path    string
}

// ParseVersion turns "v1.2.3" into [1,2,3]. It accepts a leading "v" and ignores
// a pre-release or build suffix, which is what we compare on.
func ParseVersion(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return nil, errors.New("empty version")
	}
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid version %q: %w", s, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// Compare returns -1, 0 or 1 comparing semantic versions.
func Compare(a, b string) (int, error) {
	av, err := ParseVersion(a)
	if err != nil {
		return 0, err
	}
	bv, err := ParseVersion(b)
	if err != nil {
		return 0, err
	}
	n := len(av)
	if len(bv) > n {
		n = len(bv)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(av) {
			x = av[i]
		}
		if i < len(bv) {
			y = bv[i]
		}
		if x < y {
			return -1, nil
		}
		if x > y {
			return 1, nil
		}
	}
	return 0, nil
}

// AssetName is the release asset for a platform, matching the release workflow's
// naming: motiftech2api_<version>_<goos>_<goarch>[.exe].
func AssetName(version, goos, goarch string) string {
	name := fmt.Sprintf("motiftech2api_%s_%s_%s", version, goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// LatestVersion picks the newest usable tag from a list of releases.
func LatestVersion(releases []Release, allowDraft, allowPre bool) (Release, error) {
	var candidates []Release
	for _, r := range releases {
		if r.Draft && !allowDraft {
			continue
		}
		if r.Pre && !allowPre {
			continue
		}
		if _, err := ParseVersion(r.TagName); err != nil {
			continue
		}
		candidates = append(candidates, r)
	}
	if len(candidates) == 0 {
		return Release{}, errors.New("no suitable releases found")
	}
	sort.Slice(candidates, func(i, j int) bool {
		c, _ := Compare(candidates[i].TagName, candidates[j].TagName)
		return c > 0
	})
	return candidates[0], nil
}

// FindAsset returns the download URL for name.
func FindAsset(r Release, name string) (Asset, error) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, nil
		}
	}
	var have []string
	for _, a := range r.Assets {
		have = append(have, a.Name)
	}
	sort.Strings(have)
	return Asset{}, fmt.Errorf("release %s has no asset %q (available: %s)",
		r.TagName, name, strings.Join(have, ", "))
}

// ExpectedSum extracts the checksum for fileName from a checksums.txt body.
func ExpectedSum(checksums, fileName string) (string, error) {
	for _, line := range strings.Split(checksums, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// "<hex>  <name>" (two spaces, or * for binary mode on some tools)
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		name = strings.TrimPrefix(name, "./")
		if name == fileName {
			sum := strings.ToLower(fields[0])
			if len(sum) != 64 {
				return "", fmt.Errorf("checksum for %s is not sha256 (got %d chars)", fileName, len(sum))
			}
			return sum, nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no entry for %q", fileName)
}

// verifyFile computes the SHA-256 of a file and compares it.
func verifyFile(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != strings.ToLower(want) {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, want)
	}
	return nil
}

// download writes url to path.
func download(ctx context.Context, hc *http.Client, url, path, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, res.StatusCode)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, res.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// VerifyBinary runs the candidate with --version and requires the expected value.
// Running it before replacing proves the download is not truncated or for the
// wrong architecture, which a checksum alone cannot tell you.
func VerifyBinary(path, wantVersion string) error {
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("downloaded binary failed to run (exit %d): %s",
				exitErr.ExitCode(), strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("downloaded binary could not be executed: %w", err)
	}
	got := strings.TrimSpace(string(out))
	c, err := Compare(got, wantVersion)
	if err != nil || c != 0 {
		return fmt.Errorf("downloaded binary reports %q, expected %q", got, wantVersion)
	}
	return nil
}

// CurrentIsOlder reports whether the running version should be updated to
// candidate. An unparseable running version (a plain `go build`, which reports
// "dev") is treated as older than any real release, so a source build can also
// be upgraded in place.
func CurrentIsOlder(current, candidate string) bool {
	if _, err := ParseVersion(current); err != nil {
		return true
	}
	c, err := Compare(candidate, current)
	if err != nil {
		return false
	}
	return c > 0
}

// Run performs the update. It reports whether anything changed.
func Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.HTTP == nil {
		return nil, errors.New("no HTTP client supplied")
	}
	if opts.Repo == "" {
		opts.Repo = DefaultRepo
	}
	if opts.GOOS == "" {
		opts.GOOS = runtime.GOOS
	}
	if opts.GOARCH == "" && opts.GOOS == runtime.GOOS {
		opts.GOARCH = runtime.GOARCH
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	logf := func(f string, a ...any) { fmt.Fprintf(out, f+"\n", a...) }

	rel, err := fetchLatest(ctx, opts)
	if err != nil {
		return nil, err
	}

	logf("current: %s", opts.CurrentVer)
	logf("latest:  %s", rel.TagName)

	if !CurrentIsOlder(opts.CurrentVer, rel.TagName) {
		return &Result{Updated: false, From: opts.CurrentVer, To: rel.TagName, Path: opts.Target}, nil
	}
	if opts.CheckOnly {
		return &Result{Updated: true, From: opts.CurrentVer, To: rel.TagName, Path: opts.Target}, nil
	}
	if opts.GOARCH == "" {
		return nil, fmt.Errorf("self-update is not supported for %s/%s: cannot pick a release asset",
			opts.GOOS, runtime.GOARCH)
	}

	assetName := AssetName(rel.TagName, opts.GOOS, opts.GOARCH)
	asset, err := FindAsset(rel, assetName)
	if err != nil {
		return nil, err
	}
	sumAsset, err := FindAsset(rel, "checksums.txt")
	if err != nil {
		return nil, fmt.Errorf("%w (refusing to update without a checksum)", err)
	}

	target := opts.Target
	if target == "" {
		return nil, errors.New("no target path")
	}
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}

	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".motiftech2api-update-*")
	if err != nil {
		return nil, fmt.Errorf("cannot write next to %s (try sudo, or install where the user can write): %w", target, err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	cleanup := func() { os.Remove(tmpPath) }
	defer cleanup()

	logf("downloading %s", asset.Name)
	if err := download(ctx, opts.HTTP, asset.URL, tmpPath, opts.Token); err != nil {
		return nil, err
	}
	// CreateTemp makes the file 0600. Mark it executable before running it: the
	// whole point of the next step is to prove the download actually runs.
	if err := os.Chmod(tmpPath, 0o700); err != nil {
		return nil, err
	}

	logf("verifying checksum")
	sumPath := tmpPath + ".checksums"
	if err := download(ctx, opts.HTTP, sumAsset.URL, sumPath, opts.Token); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(sumPath)
	os.Remove(sumPath)
	if err != nil {
		return nil, err
	}
	want, err := ExpectedSum(string(raw), assetName)
	if err != nil {
		return nil, err
	}
	if err := verifyFile(tmpPath, want); err != nil {
		return nil, err
	}

	logf("verifying the downloaded binary runs")
	if err := VerifyBinary(tmpPath, rel.TagName); err != nil {
		return nil, err
	}

	// Preserve the original mode; the temp file is 0755 already, but a target
	// installed 0700 should stay 0700.
	if info, err := os.Stat(target); err == nil {
		_ = os.Chmod(tmpPath, info.Mode().Perm())
	}

	logf("replacing %s", target)
	if err := os.Rename(tmpPath, target); err != nil {
		return nil, fmt.Errorf("replace %s: %w", target, err)
	}

	return &Result{Updated: true, From: opts.CurrentVer, To: rel.TagName, Path: target}, nil
}

func fetchLatest(ctx context.Context, opts Options) (Release, error) {
	base := opts.APIBase
	if base == "" {
		base = DefaultAPIBase
	}
	url := fmt.Sprintf("%s/repos/%s/releases?per_page=20", strings.TrimRight(base, "/"), opts.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "motiftech2api-updater")
	if opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+opts.Token)
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	res, err := opts.HTTP.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("query releases: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusTooManyRequests {
		return Release{}, fmt.Errorf("GitHub API rate limit hit (HTTP %d); set GITHUB_TOKEN to raise it", res.StatusCode)
	}
	if res.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("query releases: HTTP %d", res.StatusCode)
	}

	var releases []Release
	if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&releases); err != nil {
		return Release{}, fmt.Errorf("parse releases: %w", err)
	}
	return LatestVersion(releases, opts.AllowDraft, opts.AllowPre)
}
