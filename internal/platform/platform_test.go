package platform

import (
	"runtime"
	"testing"
)

func TestParse(t *testing.T) {
	for _, s := range []string{"windows/amd64", "windows/arm64", "windows/386", "linux/amd64", "linux/arm64", "linux/arm", "darwin/arm64"} {
		p, err := Parse(s)
		if err != nil || p.String() != s {
			t.Errorf("Parse(%q) = %v, %v", s, p, err)
		}
	}
	for _, s := range []string{"", "windows", "window/amd64", "linux/x86_64", "Linux/amd64", "linux/amd64/v2", "/amd64", "linux/"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) succeeded", s)
		}
	}
}

func TestCurrentIsKnownWhereGremLoaderShips(t *testing.T) {
	if _, err := Parse(Current().String()); err != nil && (runtime.GOOS == "windows" || runtime.GOOS == "linux") {
		t.Errorf("the platform of this test run is not known: %v", err)
	}
}
