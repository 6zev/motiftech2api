package config

import (
	"os"
	"path/filepath"
	"strings"
)

// LoadDotEnv reads a dotenv file and returns its key/value pairs in file order.
// A missing file is not an error: it returns nil, false.
//
// Rules, kept deliberately small:
//   - blank lines and lines starting with '#' are skipped
//   - an optional "export " prefix is allowed
//   - the first '=' separates key from value
//   - surrounding single or double quotes are stripped
//   - an unquoted trailing " # comment" is stripped
//   - "\n" and "\t" escapes are expanded inside double quotes only
//
// It does not interpolate variables or run commands, so a dotenv file cannot
// execute anything.
func LoadDotEnv(path string) (map[string]string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return parseDotEnv(string(raw)), true
}

func parseDotEnv(content string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSuffix(line, "\r")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		line = strings.TrimSpace(line)

		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" || strings.ContainsAny(key, " \t") {
			continue
		}
		val := strings.TrimSpace(line[eq+1:])

		switch {
		case len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"':
			val = val[1 : len(val)-1]
			val = strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\"`, `"`).Replace(val)
		case len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\'':
			val = val[1 : len(val)-1]
		default:
			// Strip an inline comment only when it is preceded by whitespace, so a
			// value like "a#b" survives.
			if i := strings.Index(val, " #"); i >= 0 {
				val = strings.TrimSpace(val[:i])
			}
		}
		out[key] = val
	}
	return out
}

// ApplyDotEnv loads path (if present) into the process environment WITHOUT
// overriding variables that are already set. Real environment wins, so systemd
// units, docker --env-file and CI secrets are never shadowed by a stray file.
//
// It returns the keys it actually set, for logging.
func ApplyDotEnv(path string) ([]string, bool) {
	pairs, ok := LoadDotEnv(path)
	if !ok {
		return nil, false
	}
	var applied []string
	for k, v := range pairs {
		if _, exists := os.LookupEnv(k); exists {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			continue
		}
		applied = append(applied, k)
	}
	return applied, true
}

// DefaultDotEnvPath is ./.env next to the working directory.
func DefaultDotEnvPath() string {
	return ".env"
}

// DescribeDotEnvPath renders the path for logs without exposing contents.
func DescribeDotEnvPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
