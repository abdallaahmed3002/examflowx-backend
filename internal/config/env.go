package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// LoadEnv loads environment variables from .env.local and .env in current and parent directories.
// Existing environment variables are never overwritten.
func LoadEnv() {
	dirs := []string{".", "..", "../.."}
	filenames := []string{".env.local", ".env"}

	for _, dir := range dirs {
		for _, filename := range filenames {
			path := filepath.Join(dir, filename)
			if f, err := os.Open(path); err == nil {
				scanner := bufio.NewScanner(f)
				for scanner.Scan() {
					line := strings.TrimSpace(scanner.Text())
					if line == "" || strings.HasPrefix(line, "#") {
						continue
					}
					parts := strings.SplitN(line, "=", 2)
					if len(parts) == 2 {
						key := strings.TrimSpace(parts[0])
						val := strings.TrimSpace(parts[1])
						// Trim matching quotes
						if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
							val = val[1 : len(val)-1]
						}
						if _, exists := os.LookupEnv(key); !exists {
							_ = os.Setenv(key, val)
						}
					}
				}
				_ = f.Close()
			}
		}
	}
}

