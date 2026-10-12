package pkgfile

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Template is a string with {variable} references, parsed when the package file is loaded so a
// typo is reported then, with its line, and not at three in the morning during an update.
//
// Rules:
//   - {name} is replaced by the variable's value. The value is inserted as text and never
//     expanded again, so a value that itself contains "{version}" stays literal.
//   - {{ and }} are a literal { and }. Any other lone brace is an error.
//   - Which variables a template may use depends on the setting (see the allowed sets below):
//     dir cannot use {version}, because an install directory that moves with every update would
//     leave the old one behind.
//
// Where the result is used decides what a substituted value may contain (Expand's checks): a
// value that would add a path separator or a wildcard to a file name is refused rather than
// escaped, because the asset matcher has no escape syntax and a release asset never needs one.
type Template struct {
	src   string
	parts []part
}

type part struct {
	text  string
	isVar bool
}

// Built-in variables. Users may not define these in [vars].
const (
	varName     = "name"    // package name, "photocraft"
	varNameCap  = "Name"    // with its first letter upper-cased, "Photocraft"
	varVersion  = "version" // "0.6.0"
	varTag      = "tag"     // "v0.6.0"
	varOS       = "os"      // Go's GOOS, "windows"
	varArch     = "arch"    // Go's GOARCH, "amd64"
	varAsset    = "asset"   // the selected asset's file name (checksums only)
	maxTemplate = 1024
)

var builtins = []string{varName, varNameCap, varVersion, varTag, varOS, varArch, varAsset}

// Which built-ins each kind of setting may use. User variables are allowed everywhere.
var (
	forAsset     = set(varName, varNameCap, varVersion, varTag, varOS, varArch)
	forChecksums = set(varName, varNameCap, varVersion, varTag, varOS, varArch, varAsset)
	forDir       = set(varName, varNameCap, varOS, varArch)
	forExe       = set(varName, varNameCap, varOS, varArch)
	forHook      = set(varName, varNameCap, varVersion, varTag, varOS, varArch)
)

func set(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

// parseTemplate parses src. allowed lists the built-ins the setting may use; userVars are the
// names defined in [vars] (and on the command line).
func parseTemplate(src string, allowed, userVars map[string]bool) (Template, error) {
	if len(src) > maxTemplate {
		return Template{}, fmt.Errorf("longer than %d bytes", maxTemplate)
	}
	if !utf8.ValidString(src) {
		return Template{}, errors.New("not valid UTF-8")
	}
	t := Template{src: src}
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			t.parts = append(t.parts, part{text: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '{' && i+1 < len(src) && src[i+1] == '{':
			lit.WriteByte('{')
			i++
		case c == '}' && i+1 < len(src) && src[i+1] == '}':
			lit.WriteByte('}')
			i++
		case c == '}':
			return Template{}, fmt.Errorf("lone '}' at byte %d (write }} for a literal brace)", i+1)
		case c == '{':
			end := strings.IndexByte(src[i+1:], '}')
			if end < 0 {
				return Template{}, fmt.Errorf("'{' at byte %d is not closed (write {{ for a literal brace)", i+1)
			}
			name := src[i+1 : i+1+end]
			if !validVarName(name) {
				return Template{}, fmt.Errorf("{%s} is not a variable name", name)
			}
			if !allowed[name] && !userVars[name] {
				return Template{}, unknownVar(name, allowed, userVars)
			}
			flush()
			t.parts = append(t.parts, part{text: name, isVar: true})
			i += end + 1
		default:
			lit.WriteByte(c)
		}
	}
	flush()
	return t, nil
}

func unknownVar(name string, allowed, userVars map[string]bool) error {
	if slices.Contains(builtins, name) {
		return fmt.Errorf("{%s} cannot be used here (usable: %s)", name, varList(allowed, userVars))
	}
	return fmt.Errorf("unknown variable {%s} (usable: %s; define your own under [vars])", name, varList(allowed, userVars))
}

func varList(allowed, userVars map[string]bool) string {
	names := slices.Sorted(maps.Keys(allowed))
	names = append(names, slices.Sorted(maps.Keys(userVars))...)
	for i, n := range names {
		names[i] = "{" + n + "}"
	}
	return strings.Join(names, " ")
}

func validVarName(s string) bool { return varNameRule.MatchString(s) }

// String is the template as written.
func (t Template) String() string { return t.src }

// IsZero reports whether the template is unset.
func (t Template) IsZero() bool { return t.src == "" && len(t.parts) == 0 }

// Uses reports whether the template refers to the variable.
func (t Template) Uses(name string) bool {
	for _, p := range t.parts {
		if p.isVar && p.text == name {
			return true
		}
	}
	return false
}

// expand substitutes vars. check, if not nil, vets each substituted value.
func (t Template) expand(vars map[string]string, check func(name, value string) error) (string, error) {
	var b strings.Builder
	for _, p := range t.parts {
		if !p.isVar {
			b.WriteString(p.text)
			continue
		}
		v, ok := vars[p.text]
		if !ok {
			// parseTemplate checked the name, so this is a caller that did not supply a value
			// the setting is allowed to use (e.g. {version} before a version is known).
			return "", fmt.Errorf("%q: no value for {%s} here", t.src, p.text)
		}
		if check != nil {
			if err := check(p.text, v); err != nil {
				return "", fmt.Errorf("%q: {%s} = %q %w", t.src, p.text, v, err)
			}
		}
		b.WriteString(v)
	}
	return b.String(), nil
}

// fileNameValue refuses a substituted value that would change what a file name or asset pattern
// means: a separator makes it a path, a wildcard makes it match other assets.
func fileNameValue(_, v string) error {
	if i := strings.IndexAny(v, `/\*?[]`); i >= 0 {
		return fmt.Errorf("contains %q, which is not allowed in a file name", v[i])
	}
	return controlValue("", v)
}

func controlValue(_, v string) error {
	for _, r := range v {
		if unicode.IsControl(r) {
			return errors.New("contains a control character")
		}
	}
	return nil
}

// capitalize upper-cases the first letter: "photocraft" -> "Photocraft".
func capitalize(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}
