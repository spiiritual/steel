package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A flag beats the environment, which beats .env, which beats config.json.
func TestSettingPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{"model":"file","baseURL":"https://file.example","idle":"5m","telegram":{"allowedUsers":[7]}}`), 0o600)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("OPENAI_MODEL=dotenv\nOPENAI_BASE_URL=https://dotenv.example\n"), 0o600)
	t.Setenv("OPENAI_MODEL", "env")
	t.Setenv("OPENAI_BASE_URL", "")

	fs := flag.NewFlagSet("", flag.ContinueOnError)
	o := defineFlags(fs)
	if err := fs.Parse([]string{"-idle", "1h"}); err != nil {
		t.Fatal(err)
	}
	c, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	env, err := loadEnvironment(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyConfig(fs, c, env); err != nil {
		t.Fatal(err)
	}
	if *o.model != "env" || *o.baseURL != "https://dotenv.example" || *o.idle != time.Hour || len(*o.allowedUsers) != 1 || (*o.allowedUsers)[0] != 7 {
		t.Errorf("model %q, base-url %q, idle %s, allowed users %v", *o.model, *o.baseURL, *o.idle, *o.allowedUsers)
	}
}

// The user's secrets live in secrets.env, apart from steel's own variables
// in .env; ones older versions kept in .env move there. mcpServers can
// expand them, but steel's own lookups never see them.
func TestUserSecretsMoveToSecretsEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(filepath.Join(dir, ".env"), []byte("OPENAI_API_KEY=sk-1\nOPENAI_MODEL=m\nGH_TOKEN=ghp_1\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "secrets.env"), []byte("PIN=12\n"), 0o600)

	env, err := loadEnvironment(path)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".env")); string(b) != "OPENAI_API_KEY=sk-1\nOPENAI_MODEL=m\n" {
		t.Errorf(".env:\n%s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "secrets.env")); string(b) != "PIN=12\nGH_TOKEN=ghp_1\n" {
		t.Errorf("secrets.env:\n%s", b)
	}
	if v, ok := env.expand("GH_TOKEN"); !ok || v != "ghp_1" {
		t.Errorf("expand GH_TOKEN = %q, %v", v, ok)
	}
	if _, ok := env.lookup("GH_TOKEN"); ok {
		t.Error("steel's own lookup sees a user secret")
	}
	if secretPath(path, "GH_TOKEN") != filepath.Join(dir, "secrets.env") || secretPath(path, "TELEGRAM_BOT_TOKEN") != filepath.Join(dir, ".env") {
		t.Error("config set saves to the wrong file")
	}
}
