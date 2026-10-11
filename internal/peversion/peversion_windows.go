package peversion

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ProductVersion returns the product version of the executable at path.
func ProductVersion(path string) (string, error) {
	size, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil {
		if errors.Is(err, windows.ERROR_RESOURCE_TYPE_NOT_FOUND) || errors.Is(err, windows.ERROR_RESOURCE_DATA_NOT_FOUND) ||
			errors.Is(err, windows.ERROR_BAD_EXE_FORMAT) {
			return "", fmt.Errorf("%s: %w", path, ErrNoVersion)
		}
		return "", fmt.Errorf("reading the version of %s: %w", path, err)
	}
	buf := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&buf[0])); err != nil {
		return "", fmt.Errorf("reading the version of %s: %w", path, err)
	}
	block := unsafe.Pointer(&buf[0])

	for _, lang := range languages(block) {
		if s := queryString(block, `\StringFileInfo\`+lang+`\ProductVersion`); s != "" {
			return s, nil
		}
	}

	var fixed *windows.VS_FIXEDFILEINFO
	var n uint32
	if err := windows.VerQueryValue(block, `\`, unsafe.Pointer(&fixed), &n); err == nil && fixed != nil &&
		n >= uint32(unsafe.Sizeof(*fixed)) && fixed.Signature == 0xFEEF04BD {
		if fixed.ProductVersionMS != 0 || fixed.ProductVersionLS != 0 {
			return fmt.Sprintf("%d.%d.%d.%d", fixed.ProductVersionMS>>16, fixed.ProductVersionMS&0xffff,
				fixed.ProductVersionLS>>16, fixed.ProductVersionLS&0xffff), nil
		}
	}
	return "", fmt.Errorf("%s: %w", path, ErrNoVersion)
}

// languages lists the string tables to look in: the ones the file declares, then the two that
// most files use without declaring.
func languages(block unsafe.Pointer) []string {
	var out []string
	var p unsafe.Pointer
	var n uint32
	if err := windows.VerQueryValue(block, `\VarFileInfo\Translation`, unsafe.Pointer(&p), &n); err == nil && p != nil {
		pairs := unsafe.Slice((*uint16)(p), n/2)
		for i := 0; i+1 < len(pairs); i += 2 {
			out = append(out, fmt.Sprintf("%04x%04x", pairs[i], pairs[i+1]))
		}
	}
	return append(out, "040904b0", "040904e4")
}

func queryString(block unsafe.Pointer, sub string) string {
	var p *uint16
	var n uint32 // characters, including the terminating NUL
	if err := windows.VerQueryValue(block, sub, unsafe.Pointer(&p), &n); err != nil || p == nil || n == 0 {
		return ""
	}
	return strings.TrimSpace(windows.UTF16PtrToString(p))
}
