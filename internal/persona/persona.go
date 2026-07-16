// Package persona reads/writes the DJ persona bible — a committed product
// artifact (persona/tieu-duong-duong.md), editable from the console.
package persona

import (
	"os"
	"path/filepath"
)

const filename = "tieu-duong-duong.md"

func Load(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, filename))
	return string(b), err
}

func Save(dir, content string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644)
}
