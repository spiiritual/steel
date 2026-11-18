package secret

import (
	"os"
	"path/filepath"
	"testing"
)

// Secrets with awkward characters survive a save, and editing one keeps the
// rest of the file.
func TestDotenvRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte("# keys\nexport A='single'\nB=old\n"), 0o600)
	d, err := ReadDotenv(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Set("B", `p@ss "w#rd" \x`)
	d.Set("C", "plain")
	if err := d.Write(path); err != nil {
		t.Fatal(err)
	}
	if d, err = ReadDotenv(path); err != nil {
		t.Fatal(err)
	}
	got := d.Vars()
	if got["A"] != "single" || got["B"] != `p@ss "w#rd" \x` || got["C"] != "plain" || len(got) != 3 {
		t.Errorf("vars = %q", got)
	}
	if d.lines[0] != "# keys" {
		t.Errorf("comment lost: %q", d.lines)
	}
}
