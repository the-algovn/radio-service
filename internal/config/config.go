// Package config loads lab configuration: ~/.algovn/radio-lab.env first
// (KEY=VALUE lines, # comments), then process env, which always wins.
package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Load reads ~/.algovn/radio-lab.env if present. Missing file is fine.
func Load() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	f := filepath.Join(home, ".algovn", "radio-lab.env")
	if _, err := os.Stat(f); err != nil {
		return nil
	}
	return loadFile(f)
}

func loadFile(path string) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if os.Getenv(k) == "" {
			os.Setenv(k, strings.TrimSpace(v))
		}
	}
	return sc.Err()
}

// Get returns the env value or def when unset/empty.
func Get(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
