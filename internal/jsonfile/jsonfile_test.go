package jsonfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type doc struct {
	SchemaVersion int    `json:"schema_version"`
	Name          string `json:"name"`
}

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "x.json")
	if err := Write(path, doc{SchemaVersion: 1, Name: "a"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.HasSuffix(string(raw), "}\n") || !strings.Contains(string(raw), "\n  \"name\"") {
		t.Errorf("not indented with a final newline: %q", raw)
	}
	var d doc
	found, err := Read(path, 1, &d)
	if err != nil || !found || d.Name != "a" {
		t.Fatalf("Read = %v, %v, %+v", found, err, d)
	}
}

func TestMissingFileIsNotAnError(t *testing.T) {
	var d doc
	found, err := Read(filepath.Join(t.TempDir(), "none.json"), 1, &d)
	if found || err != nil {
		t.Errorf("Read of a missing file = %v, %v", found, err)
	}
}

func TestRules(t *testing.T) {
	cases := []struct {
		name, data string
		want       error
	}{
		{"ok", `{"schema_version":1,"name":"x"}`, nil},
		{"older is fine", `{"schema_version":1}`, nil},
		{"newer", `{"schema_version":2,"name":"x"}`, ErrNewerSchema},
		// A newer file with new fields must say "newer", not "unknown field".
		{"newer with new fields", `{"schema_version":3,"brand_new":true}`, ErrNewerSchema},
		{"missing version", `{"name":"x"}`, ErrCorrupt},
		{"zero version", `{"schema_version":0}`, ErrCorrupt},
		{"string version", `{"schema_version":"1"}`, ErrCorrupt},
		{"fraction version", `{"schema_version":1.5}`, ErrCorrupt},
		{"unknown field", `{"schema_version":1,"title":"x"}`, ErrCorrupt},
		{"wrong type", `{"schema_version":1,"name":7}`, ErrCorrupt},
		{"truncated", `{"schema_version":1,"name":"x`, ErrCorrupt},
		{"trailing data", `{"schema_version":1}{"schema_version":1}`, ErrCorrupt},
		{"trailing junk", `{"schema_version":1} x`, ErrCorrupt},
		{"empty", ``, ErrCorrupt},
		{"array", `[]`, ErrCorrupt},
	}
	for _, c := range cases {
		var d doc
		err := Decode("f.json", []byte(c.data), 1, &d)
		if (c.want == nil) != (err == nil) || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("%s: Decode = %v, want %v", c.name, err, c.want)
		}
		if err != nil && !strings.Contains(err.Error(), "f.json") {
			t.Errorf("%s: message does not name the file: %v", c.name, err)
		}
	}
}

func TestTrailingWhitespaceIsFine(t *testing.T) {
	var d doc
	if err := Decode("f", []byte("{\"schema_version\":1}\r\n\n "), 1, &d); err != nil {
		t.Error(err)
	}
}

func TestOversizedFileIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.json")
	big := `{"schema_version":1,"name":"` + strings.Repeat("x", MaxBytes) + `"}`
	if err := os.WriteFile(path, []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	var d doc
	if _, err := Read(path, 1, &d); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Read of an oversized file = %v, want ErrCorrupt", err)
	}
}
