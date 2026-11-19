package secret

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testStore(t *testing.T, env string, o Options) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if env != "" {
		os.WriteFile(path, []byte(env), 0o600)
	}
	s, err := Open(path, o)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

// Values go into a call's JSON arguments escaped, so a value with quotes
// or backslashes can't break out of its string.
func TestFillEscapes(t *testing.T) {
	s, _ := testStore(t, `TOKEN="ab\"c\\d"`+"\nOTHER=xyz123\n", Options{})
	got, names, err := s.Fill([]byte(`{"cmd":"curl -H '{{secret:TOKEN}}' {{secret:OTHER}} {{secret:TOKEN}}"}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"cmd":"curl -H 'ab\"c\\d' xyz123 ab\"c\\d"}`; string(got) != want {
		t.Errorf("filled %s, want %s", got, want)
	}
	if strings.Join(names, ",") != "TOKEN,OTHER" {
		t.Errorf("names %v", names)
	}
}

// A call naming a missing secret mustn't run at all.
func TestFillUnknown(t *testing.T) {
	s, _ := testStore(t, "", Options{})
	if _, _, err := s.Fill([]byte(`{"a":"{{secret:MISSING}}"}`)); err == nil || !strings.Contains(err.Error(), "no secret MISSING") {
		t.Errorf("err = %v", err)
	}
}

// Every value is redacted, however short, longest first; extra values
// (steel's own tokens) too, though they aren't secrets the model can use.
func TestRedact(t *testing.T) {
	s, _ := testStore(t, "PIN=4821\nTOKEN=tok_123\nLONG=tok_123456\nQUOTED=\"a<b&c\"\n",
		Options{Redact: map[string]string{"BOT": "bot-secret"}})
	in := `tok_123456 tok_123 4821 {"q":"a<b&c"} a<b&c bot-secret`
	want := `{{secret:LONG}} {{secret:TOKEN}} {{secret:PIN}} {"q":"{{secret:QUOTED}}"} {{secret:QUOTED}} {{secret:BOT}}`
	if got := s.Redact(in); got != want {
		t.Errorf("Redact:\n got %s\nwant %s", got, want)
	}
	if _, _, err := s.Fill([]byte(`{"a":"{{secret:BOT}}"}`)); err == nil {
		t.Error("an extra value was filled in")
	}
}

// Secrets set on the host while steel runs are used, and saving keeps
// them.
func TestHostEdits(t *testing.T) {
	s, path := testStore(t, "", Options{})
	os.WriteFile(path, []byte("# mine\nHOST=from-host\n"), 0o600)
	if got := s.Redact("from-host"); got != "{{secret:HOST}}" {
		t.Errorf("host edit not seen: %s", got)
	}
	if err := s.Set("ADDED", "from-tool"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "# mine\nHOST=from-host\nADDED=from-tool\n" {
		t.Errorf("file:\n%s", b)
	}
	if names, _ := s.Names(); strings.Join(names, ",") != "ADDED,HOST" {
		t.Errorf("names %v", names)
	}
	if ok, err := s.Remove("HOST"); !ok || err != nil {
		t.Errorf("Remove = %v, %v", ok, err)
	}
	if got := s.Redact("from-host"); got != "from-host" {
		t.Errorf("removed secret still redacted: %s", got)
	}
}

func TestSetRefuses(t *testing.T) {
	s, _ := testStore(t, "", Options{})
	for _, c := range []struct{ name, value string }{
		{"lower", "value123"},
		{"EMPTY", ""},
	} {
		if err := s.Set(c.name, c.value); err == nil {
			t.Errorf("Set(%q, %q) saved", c.name, c.value)
		}
	}
	if err := s.Set("PIN", "12"); err != nil {
		t.Errorf("a short secret wasn't saved: %v", err)
	}
}

// Redacted JSON still parses: a value in a number or other literal turns
// it into a string, and one matching part of an escape doesn't break it.
func TestRedactJSON(t *testing.T) {
	s, _ := testStore(t, "PIN=0041\nTOKEN=tok_\"q\n", Options{})
	in := `{"n":41,"pin":0041,"m":[100410,"code 0041"],"e":"\u0041","t":"tok_\"q","0041":true}`
	want := `{"n":41,"pin":"{{secret:PIN}}","m":["1{{secret:PIN}}0","code {{secret:PIN}}"],"e":"\u0041","t":"{{secret:TOKEN}}","{{secret:PIN}}":true}`
	got := s.RedactJSON(in)
	if got != want {
		t.Errorf("RedactJSON:\n got %s\nwant %s", got, want)
	}
	if !json.Valid([]byte(got)) {
		t.Errorf("not JSON: %s", got)
	}
}

func TestSetRedactedReplacesValue(t *testing.T) {
	s, _ := testStore(t, "", Options{})
	s.SetRedacted("TOKEN", "first")
	s.SetRedacted("TOKEN", "second")
	if got := s.Redact("first second"); got != "first {{secret:TOKEN}}" {
		t.Errorf("Redact = %q", got)
	}
}
