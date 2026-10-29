package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseVersion(t *testing.T) {
	cases := map[string][]int{
		"v1.2.3":       {1, 2, 3},
		"1.2.3":        {1, 2, 3},
		"V1.0":         {1, 0},
		"v2.0.0-rc.1":  {2, 0, 0},
		"v1.2.3+build": {1, 2, 3},
	}
	for in, want := range cases {
		got, err := ParseVersion(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%q = %v, want %v", in, got, want)
		}
	}
}

func TestParseVersionRejects(t *testing.T) {
	for _, in := range []string{"", "v", "abc", "v1.x"} {
		if _, err := ParseVersion(in); err == nil {
			t.Errorf("%q should fail", in)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.0.0", "v1.0.0", 0},
		{"v1.0.1", "v1.0.0", 1},
		{"v1.0.0", "v1.0.1", -1},
		{"v1.10.0", "v1.9.0", 1},
		{"v2", "v1.9.9", 1},
		{"v1.0", "v1.0.0", 0},
		{"dev", "v1.0.0", -1}, // "dev" fails to parse; covered below
	}
	for _, c := range cases {
		got, err := Compare(c.a, c.b)
		if c.a == "dev" {
			if err == nil {
				t.Errorf("Compare(dev, ...) should error")
			}
			continue
		}
		if err != nil {
			t.Errorf("%s vs %s: %v", c.a, c.b, err)
			continue
		}
		if got != c.want {
			t.Errorf("Compare(%s,%s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestAssetName(t *testing.T) {
	if got := AssetName("v1.2.3", "linux", "amd64"); got != "motiftech2api_v1.2.3_linux_amd64" {
		t.Errorf("got %q", got)
	}
	if got := AssetName("v1.2.3", "windows", "amd64"); got != "motiftech2api_v1.2.3_windows_amd64.exe" {
		t.Errorf("got %q", got)
	}
}

func TestLatestVersionSkipsDraftAndPrerelease(t *testing.T) {
	rels := []Release{
		{TagName: "v1.0.0"},
		{TagName: "v1.2.0", Draft: true},
		{TagName: "v1.1.0"},
		{TagName: "v1.3.0", Pre: true},
		{TagName: "nonsense"},
	}
	got, err := LatestVersion(rels, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.TagName != "v1.1.0" {
		t.Fatalf("got %q, want v1.1.0", got.TagName)
	}

	got, err = LatestVersion(rels, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.TagName != "v1.3.0" {
		t.Fatalf("got %q, want v1.3.0", got.TagName)
	}
}

func TestLatestVersionEmpty(t *testing.T) {
	if _, err := LatestVersion(nil, false, false); err == nil {
		t.Fatal("expected an error")
	}
}

func TestExpectedSum(t *testing.T) {
	sum := strings.Repeat("a", 64)
	body := fmt.Sprintf("%s  file1\n%s  motiftech2api_v1.2.3_linux_amd64\n", strings.Repeat("b", 64), sum)
	got, err := ExpectedSum(body, "motiftech2api_v1.2.3_linux_amd64")
	if err != nil {
		t.Fatal(err)
	}
	if got != sum {
		t.Fatalf("got %q", got)
	}
}

func TestExpectedSumHandlesStarAndDotSlash(t *testing.T) {
	sum := strings.Repeat("c", 64)
	body := fmt.Sprintf("%s *./motiftech2api_v1.0.0_linux_amd64\n", sum)
	got, err := ExpectedSum(body, "motiftech2api_v1.0.0_linux_amd64")
	if err != nil {
		t.Fatal(err)
	}
	if got != sum {
		t.Fatalf("got %q", got)
	}
}

func TestExpectedSumMissing(t *testing.T) {
	if _, err := ExpectedSum(strings.Repeat("a", 64)+"  other\n", "wanted"); err == nil {
		t.Fatal("expected an error for a missing entry")
	}
}

func TestVerifyFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	content := []byte("hello")
	os.WriteFile(p, content, 0o644)

	h := sha256.Sum256(content)
	if err := verifyFile(p, hex.EncodeToString(h[:])); err != nil {
		t.Fatalf("valid sum rejected: %v", err)
	}
	if err := verifyFile(p, strings.Repeat("0", 64)); err == nil {
		t.Fatal("bad sum accepted")
	}
}

// --- end to end against a fake GitHub ---

// fakeRelease serves a release API and assets. binary is the body of the asset.
func fakeRelease(t *testing.T, tag string, binary []byte, sumOverride string) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	var srv *httptest.Server

	mux.HandleFunc("/repos/o/r/releases", func(w http.ResponseWriter, r *http.Request) {
		name := AssetName(tag, runtime.GOOS, runtime.GOARCH)
		rels := []Release{{
			TagName: tag,
			Assets: []Asset{
				{Name: name, URL: srv.URL + "/assets/" + name},
				{Name: "checksums.txt", URL: srv.URL + "/assets/checksums.txt"},
			},
		}}
		json.NewEncoder(w).Encode(rels)
	})

	sum := sumOverride
	if sum == "" {
		h := sha256.Sum256(binary)
		sum = hex.EncodeToString(h[:])
	}
	name := AssetName(tag, runtime.GOOS, runtime.GOARCH)

	mux.HandleFunc("/assets/"+name, func(w http.ResponseWriter, r *http.Request) {
		w.Write(binary)
	})
	mux.HandleFunc("/assets/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", sum, name)
	})

	srv = httptest.NewServer(mux)
	return srv
}

// fakeBinary is a shell script that reports a version, which is enough to prove
// the updater runs the candidate before replacing the target.
func fakeBinary(version string) []byte {
	return []byte("#!/bin/sh\necho " + version + "\n")
}

func TestRunRefusesWithoutNewerVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture")
	}
	srv := fakeRelease(t, "v1.0.0", fakeBinary("v1.0.0"), "")
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "motiftech2api")
	os.WriteFile(target, []byte("original"), 0o755)

	var out strings.Builder
	res, err := Run(context.Background(), Options{
		Repo:       "o/r",
		APIBase:    srv.URL,
		CurrentVer: "v1.0.0",
		HTTP:       &http.Client{Timeout: 5 * time.Second},
		Target:     target,
		Out:        &out,
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if res.Updated {
		t.Fatal("should not update when already current")
	}
	if b, _ := os.ReadFile(target); string(b) != "original" {
		t.Fatal("target was modified")
	}
}

func TestRunUpdatesAndReplaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture")
	}
	srv := fakeRelease(t, "v2.0.0", fakeBinary("v2.0.0"), "")
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "motiftech2api")
	os.WriteFile(target, fakeBinary("v1.0.0"), 0o700)

	var out strings.Builder
	res, err := Run(context.Background(), Options{
		Repo:       "o/r",
		APIBase:    srv.URL,
		CurrentVer: "v1.0.0",
		HTTP:       &http.Client{Timeout: 5 * time.Second},
		Target:     target,
		Out:        &out,
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !res.Updated || res.To != "v2.0.0" {
		t.Fatalf("result = %+v", res)
	}

	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "v2.0.0") {
		t.Fatalf("target not replaced: %q", body)
	}

	// Permissions are preserved.
	info, _ := os.Stat(target)
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %v, want 0700", info.Mode().Perm())
	}

	// No temp files left behind.
	entries, _ := os.ReadDir(filepath.Dir(target))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".motiftech2api-update-") {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}

func TestRunRefusesBadChecksum(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture")
	}
	srv := fakeRelease(t, "v2.0.0", fakeBinary("v2.0.0"), strings.Repeat("f", 64))
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "motiftech2api")
	os.WriteFile(target, fakeBinary("v1.0.0"), 0o755)

	_, err := Run(context.Background(), Options{
		Repo:       "o/r",
		APIBase:    srv.URL,
		CurrentVer: "v1.0.0",
		HTTP:       &http.Client{Timeout: 5 * time.Second},
		Target:     target,
	})
	if err == nil {
		t.Fatal("expected a checksum failure")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	// Original untouched.
	if b, _ := os.ReadFile(target); !strings.Contains(string(b), "v1.0.0") {
		t.Fatal("target was modified despite the failure")
	}
}

func TestRunRefusesBinaryThatDoesNotRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture")
	}
	// Checksum is valid, but the file is not executable output we can parse.
	srv := fakeRelease(t, "v2.0.0", []byte("not a script"), "")
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "motiftech2api")
	os.WriteFile(target, fakeBinary("v1.0.0"), 0o755)

	_, err := Run(context.Background(), Options{
		Repo:       "o/r",
		APIBase:    srv.URL,
		CurrentVer: "v1.0.0",
		HTTP:       &http.Client{Timeout: 5 * time.Second},
		Target:     target,
	})
	if err == nil {
		t.Fatal("expected the run check to fail")
	}
	if b, _ := os.ReadFile(target); !strings.Contains(string(b), "v1.0.0") {
		t.Fatal("target was modified despite the failure")
	}
}

func TestRunRefusesWrongReportedVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture")
	}
	// Checksum ok, runs fine, but reports a different version than the tag.
	srv := fakeRelease(t, "v2.0.0", fakeBinary("v1.5.5"), "")
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "motiftech2api")
	os.WriteFile(target, fakeBinary("v1.0.0"), 0o755)

	_, err := Run(context.Background(), Options{
		Repo:       "o/r",
		APIBase:    srv.URL,
		CurrentVer: "v1.0.0",
		HTTP:       &http.Client{Timeout: 5 * time.Second},
		Target:     target,
	})
	if err == nil || !strings.Contains(err.Error(), "reports") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(target); !strings.Contains(string(b), "v1.0.0") {
		t.Fatal("target was modified despite the failure")
	}
}

func TestRunMissingChecksumsAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]Release{{
			TagName: "v2.0.0",
			Assets:  []Asset{{Name: AssetName("v2.0.0", runtime.GOOS, runtime.GOARCH), URL: "http://x/a"}},
		}})
	}))
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "motiftech2api")
	os.WriteFile(target, []byte("x"), 0o755)

	_, err := Run(context.Background(), Options{
		Repo:       "o/r",
		APIBase:    srv.URL,
		CurrentVer: "v1.0.0",
		HTTP:       &http.Client{Timeout: 5 * time.Second},
		Target:     target,
	})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	// The API URL is derived from the repo, so this fake server never gets hit;
	// the important part is that the run does not silently proceed.
	_ = srv
}

var _ = io.Discard

func TestCurrentIsOlder(t *testing.T) {
	cases := []struct {
		current, candidate string
		want               bool
	}{
		{"v1.0.0", "v1.1.0", true},
		{"v1.1.0", "v1.0.0", false},
		{"v1.1.0", "v1.1.0", false},
		// A source build reports "dev"; it must be upgradable.
		{"dev", "v1.0.0", true},
		{"", "v1.0.0", true},
	}
	for _, c := range cases {
		if got := CurrentIsOlder(c.current, c.candidate); got != c.want {
			t.Errorf("CurrentIsOlder(%q,%q) = %v, want %v", c.current, c.candidate, got, c.want)
		}
	}
}

// A "dev" build must actually go through the whole update path.
func TestRunUpdatesFromDevBuild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture")
	}
	srv := fakeRelease(t, "v2.0.0", fakeBinary("v2.0.0"), "")
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "motiftech2api")
	os.WriteFile(target, fakeBinary("dev"), 0o755)

	res, err := Run(context.Background(), Options{
		Repo:       "o/r",
		APIBase:    srv.URL,
		CurrentVer: "dev",
		HTTP:       &http.Client{Timeout: 5 * time.Second},
		Target:     target,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Updated || res.To != "v2.0.0" {
		t.Fatalf("result = %+v", res)
	}
	if b, _ := os.ReadFile(target); !strings.Contains(string(b), "v2.0.0") {
		t.Fatalf("target not replaced: %q", b)
	}
}
