package maintenance

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Inspect the original components before cleaning: cleaning would hide links
// followed by .., and would turn forbidden dot components into safe-looking paths.
func safePath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("Not an absolute path: %s", path)
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		if part == "." || part == ".." {
			return "", fmt.Errorf("Unsafe path: %s", path)
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("inspect path %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("Symbolic link in path: %s", current)
		}
	}
	return filepath.Clean(path), nil
}

func regularFile(path string) error {
	path, err := safePath(path)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("Missing regular file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("Missing regular file: %s", path)
	}
	return nil
}

func readRegularFile(path string) ([]byte, error) {
	if err := regularFile(path); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func containsPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func outsideDirectory(root, path, other string) (string, error) {
	path, err := safePath(path)
	if err != nil {
		return "", err
	}
	root, err = safePath(root)
	if err != nil {
		return "", err
	}
	if containsPath(root, path) {
		return "", fmt.Errorf("Output inside checkout: %s", path)
	}
	if other != "" {
		other, err = safePath(other)
		if err != nil {
			return "", err
		}
		if containsPath(other, path) || containsPath(path, other) {
			return "", fmt.Errorf("Overlapping directories")
		}
	}
	entries, err := os.ReadDir(path)
	if os.IsNotExist(err) {
		return path, nil
	}
	if err != nil {
		return "", fmt.Errorf("output directory %s: %w", path, err)
	}
	if len(entries) != 0 {
		return "", fmt.Errorf("Directory is not empty: %s", path)
	}
	return path, nil
}

func copyFile(src, dst string) error {
	if err := regularFile(src); err != nil {
		return err
	}
	dst, err := safePath(dst)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(dst); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("Not a regular destination: %s", dst)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func writeRegularFile(path string, data []byte) error {
	path, err := safePath(path)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("Not a regular destination: %s", path)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func fileHash(path string) (*string, error) {
	path, err := safePath(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("Not a regular file: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return nil, err
	}
	value := hex.EncodeToString(h.Sum(nil))
	return &value, nil
}

func hashBytes(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func hashEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
