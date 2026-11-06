package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnv(t *testing.T) {
	tempDir := t.TempDir()
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWd) }()

	if err := os.Chdir(tempDir); err != nil {
		t.Fatal(err)
	}

	envFile := filepath.Join(tempDir, ".env.local")
	content := "TEST_ENV_KEY_1=hello\nTEST_ENV_KEY_2=\"world\"\n# Comment line\nTEST_ENV_KEY_3='examflow'\n"
	if err := os.WriteFile(envFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	_ = os.Unsetenv("TEST_ENV_KEY_1")
	_ = os.Unsetenv("TEST_ENV_KEY_2")
	_ = os.Unsetenv("TEST_ENV_KEY_3")

	LoadEnv()

	if os.Getenv("TEST_ENV_KEY_1") != "hello" {
		t.Errorf("expected 'hello', got '%s'", os.Getenv("TEST_ENV_KEY_1"))
	}
	if os.Getenv("TEST_ENV_KEY_2") != "world" {
		t.Errorf("expected 'world', got '%s'", os.Getenv("TEST_ENV_KEY_2"))
	}
	if os.Getenv("TEST_ENV_KEY_3") != "examflow" {
		t.Errorf("expected 'examflow', got '%s'", os.Getenv("TEST_ENV_KEY_3"))
	}
}

