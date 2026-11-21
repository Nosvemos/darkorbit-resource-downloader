package integrity

import (
	"crypto/md5"
	"encoding/hex"
	"io"
	"os"
	"strings"
)

// FileMatchesManifestHash reports whether the file at filePath matches the
// manifest hash. An empty manifest hash never matches.
func FileMatchesManifestHash(filePath, manifestHash string) (bool, error) {
	if manifestHash == "" {
		return false, nil
	}
	sum, err := FileMD5(filePath)
	if err != nil {
		return false, err
	}
	return MatchesManifestHash(sum, manifestHash), nil
}

// MatchesManifestHash compares a full hex MD5 digest against a manifest hash.
//
// DarkOrbit manifests publish MD5 digests with the last byte replaced by
// "00". When the manifest hash ends in "00" the digest is normalized the same
// way before comparing; otherwise the full digest is compared. Comparison is
// case-insensitive. An empty manifest hash never matches.
func MatchesManifestHash(md5Hex, manifestHash string) bool {
	if manifestHash == "" || md5Hex == "" {
		return false
	}
	if strings.HasSuffix(manifestHash, "00") {
		return strings.EqualFold(Normalize(md5Hex), manifestHash)
	}
	return strings.EqualFold(md5Hex, manifestHash)
}

// Normalize replaces the last byte of a hex MD5 digest with "00", matching
// the form used in DarkOrbit manifests.
func Normalize(md5Hex string) string {
	if len(md5Hex) < 2 {
		return md5Hex
	}
	return md5Hex[:len(md5Hex)-2] + "00"
}

// FileMD5 returns the full lowercase hex MD5 digest of the file at filePath.
func FileMD5(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hasher := md5.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// NormalizedFileMD5 returns the MD5 digest of the file at filePath with its
// last byte replaced by "00".
func NormalizedFileMD5(filePath string) (string, error) {
	sum, err := FileMD5(filePath)
	if err != nil {
		return "", err
	}
	return Normalize(sum), nil
}
