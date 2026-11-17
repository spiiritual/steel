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

// Secrets with awkward characters survive a save, and editing one keeps the
// rest of the file.
func TestDotenvRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte("# keys\nexport A='single'\nB=old\n"), 0o600)
	d, err := readDotenv(path)
	if err != nil {
		t.Fatal(err)
	}
	d.set("B", `p@ss "w#rd" \x`)
	d.set("C", "plain")
	if err := d.write(path); err != nil {
		t.Fatal(err)
	}
	if d, err = readDotenv(path); err != nil {
		t.Fatal(err)
	}
	got := d.vars()
	if got["A"] != "single" || got["B"] != `p@ss "w#rd" \x` || got["C"] != "plain" || len(got) != 3 {
		t.Errorf("vars = %q", got)
	}
	if d.lines[0] != "# keys" {
		t.Errorf("comment lost: %q", d.lines)
	}
}
