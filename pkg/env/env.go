// Package env loads a .env file into the process environment.
//
// Format: KEY=VALUE, one per line. Blank lines and # comments are ignored.
// Does NOT overwrite existing env vars (shell exports take priority).
// Looks for .env in the current directory, then next to the binary.
package env

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Load reads .env from the current directory (or next to the binary)
// and sets any keys that aren't already in the environment.
func Load() {
	paths := []string{".env"}
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), ".env"))
	}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			// Strip inline comments (after first # not in quotes).
			if idx := strings.Index(line, " #"); idx >= 0 {
				line = strings.TrimSpace(line[:idx])
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			// Strip surrounding quotes if present.
			if len(val) >= 2 && val[0] == val[len(val)-1] && (val[0] == '"' || val[0] == '\'') {
				val = val[1 : len(val)-1]
			}
			if key != "" && os.Getenv(key) == "" {
				os.Setenv(key, val)
			}
		}
		f.Close()
		return
	}
}
