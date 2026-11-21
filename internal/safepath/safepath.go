// Package safepath resolves server-supplied relative paths under a local root
// without allowing them to escape it.
package safepath

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrUnsafePath is returned (wrapped) by Join when a relative path is rejected.
var ErrUnsafePath = errors.New("unsafe relative path")

// Join resolves rel, a forward-slash relative path supplied by the server,
// under root. It rejects empty paths, backslashes, colons, absolute paths,
// paths that escape root via "..", paths that resolve to root itself, and
// (on Windows) reserved device names such as NUL or COM1.
func Join(root, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("%w: empty", ErrUnsafePath)
	}
	if strings.ContainsAny(rel, `\:`) {
		return "", fmt.Errorf("%w: %q contains a backslash or colon", ErrUnsafePath, rel)
	}
	local := filepath.FromSlash(rel)
	if !filepath.IsLocal(local) {
		return "", fmt.Errorf("%w: %q is not a local path", ErrUnsafePath, rel)
	}
	if filepath.Clean(local) == "." {
		return "", fmt.Errorf("%w: %q refers to the root directory", ErrUnsafePath, rel)
	}
	return filepath.Join(root, local), nil
}
