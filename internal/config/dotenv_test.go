package config

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseBasic(t *testing.T) {
	got := parseDotEnv("PORT=3000\nHOST=1.2.3.4\n")
	if got["PORT"] != "3000" || got["HOST"] != "1.2.3.4" {
		t.Fatalf("got %v", got)
	}
}

func TestParseSkipsCommentsAndBlanks(t *testing.T) {
	got := parseDotEnv("# a comment\n\n   \nPORT=3000\n  # indented comment\n")
	if len(got) != 1 || got["PORT"] != "3000" {
		t.Fatalf("got %v", got)
	}
}

func TestParseExportPrefix(t *testing.T) {
	got := parseDotEnv("export PORT=3000\n")
	if got["PORT"] != "3000" {
		t.Fatalf("got %v", got)
	}
}

func TestParseQuotes(t *testing.T) {
	got := parseDotEnv(`A="hello world"` + "\n" + `B='single quoted'` + "\n" + `C="with \"quotes\""` + "\n")
	if got["A"] != "hello world" {
		t.Errorf("A = %q", got["A"])
	}
	if got["B"] != "single quoted" {
		t.Errorf("B = %q", got["B"])
	}
	if got["C"] != `with "quotes"` {
		t.Errorf("C = %q", got["C"])
	}
}

func TestParseEmptyValue(t *testing.T) {
	got := parseDotEnv("GATEWAY_API_KEYS=\n")
	if v, ok := got["GATEWAY_API_KEYS"]; !ok || v != "" {
		t.Fatalf("got %q ok=%v", v, ok)
	}
}

func TestParseInlineComment(t *testing.T) {
	got := parseDotEnv("PORT=3000 # the port\nURL=https://x/a#frag\n")
	if got["PORT"] != "3000" {
		t.Errorf("PORT = %q, want 3000", got["PORT"])
	}
	// A '#': not preceded by a space is part of the value.
	if got["URL"] != "https://x/a#frag" {
		t.Errorf("URL = %q", got["URL"])
	}
}

func TestParseIgnoresJunk(t *testing.T) {
	got := parseDotEnv("noequalshere\n=novalue\nBAD KEY=x\n")
	if len(got) != 0 {
		t.Fatalf("expected nothing, got %v", got)
	}
}

func TestParseValueWithEquals(t *testing.T) {
	got := parseDotEnv("URL=https://h/p?a=1&b=2\n")
	if got["URL"] != "https://h/p?a=1&b=2" {
		t.Fatalf("got %q", got["URL"])
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, ok := LoadDotEnv(filepath.Join(t.TempDir(), "nope.env")); ok {
		t.Fatal("missing file should report false")
	}
}

// The important property: a real environment variable must win over the file.
func TestApplyDoesNotOverrideRealEnv(t *testing.T) {
	p := writeTemp(t, "MOTIF_TEST_ALPHA=from_file\nMOTIF_TEST_BETA=from_file\n")
	t.Cleanup(func() {
		os.Unsetenv("MOTIF_TEST_ALPHA")
		os.Unsetenv("MOTIF_TEST_BETA")
	})

	os.Setenv("MOTIF_TEST_ALPHA", "from_env")
	os.Unsetenv("MOTIF_TEST_BETA")

	applied, ok := ApplyDotEnv(p)
	if !ok {
		t.Fatal("expected the file to be found")
	}

	if got := os.Getenv("MOTIF_TEST_ALPHA"); got != "from_env" {
		t.Errorf("ALPHA = %q, want from_env (real env must win)", got)
	}
	if got := os.Getenv("MOTIF_TEST_BETA"); got != "from_file" {
		t.Errorf("BETA = %q, want from_file", got)
	}

	sort.Strings(applied)
	if len(applied) != 1 || applied[0] != "MOTIF_TEST_BETA" {
		t.Errorf("applied = %v, want [MOTIF_TEST_BETA]", applied)
	}
}

func TestApplyMissingFileIsNoop(t *testing.T) {
	applied, ok := ApplyDotEnv(filepath.Join(t.TempDir(), "absent.env"))
	if ok || applied != nil {
		t.Fatalf("applied=%v ok=%v", applied, ok)
	}
}

// A dotenv file must not be able to run commands or interpolate.
func TestParseDoesNotInterpolateOrExecute(t *testing.T) {
	got := parseDotEnv("A=$HOME\nB=$(whoami)\nC=`id`\n")
	if got["A"] != "$HOME" || got["B"] != "$(whoami)" || got["C"] != "`id`" {
		t.Fatalf("values were unexpectedly expanded: %v", got)
	}
}
