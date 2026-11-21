package manifest

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/model"
)

type FileCollection struct {
	Locations []LocationNode `xml:"location"`
	Files     []FileNode     `xml:"file"`
}

type LocationNode struct {
	ID   string `xml:"id,attr"`
	Path string `xml:"path,attr"`
}

type FileNode struct {
	ID       string `xml:"id,attr"`
	Location string `xml:"location,attr"`
	Name     string `xml:"name,attr"`
	Type     string `xml:"type,attr"`
	Hash     string `xml:"hash,attr"`
}

// SkippedFile is a <file> entry that was left out of the result because it
// had no name or because its path would escape the output directory.
type SkippedFile struct {
	ID     string
	Path   string
	Reason string
}

// LoadResult holds the resources of one filecollection manifest and the
// entries that were rejected while building them.
type LoadResult struct {
	Resources []model.Resource
	Skipped   []SkippedFile
}

// LoadResourcesFromFile is Load without the skipped-entry report.
func LoadResourcesFromFile(localPath, manifestRelPath, baseURL, category string) ([]model.Resource, bool, error) {
	result, ok, err := Load(localPath, manifestRelPath, baseURL, category)
	return result.Resources, ok, err
}

// Load parses a filecollection manifest. ok is false (with a nil error) when
// the file is not a filecollection document at all, for example a SWF or an
// HTML error page. A document that is a filecollection but fails to parse, or
// that declares itself as XML and breaks before its root element, returns an
// error wrapped with the manifest path.
func Load(localPath, manifestRelPath, baseURL, category string) (LoadResult, bool, error) {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return LoadResult{}, false, err
	}

	collection, ok, err := decodeFileCollection(data)
	if err != nil {
		return LoadResult{}, false, fmt.Errorf("parse manifest %s: %w", manifestRelPath, err)
	}
	if !ok {
		return LoadResult{}, false, nil
	}

	locationMap := make(map[string]string, len(collection.Locations))
	for _, location := range collection.Locations {
		locationMap[location.ID] = location.Path
	}

	basePrefix := collectionBasePrefix(manifestRelPath)
	result := LoadResult{Resources: make([]model.Resource, 0, len(collection.Files))}
	for _, file := range collection.Files {
		locationPath, declared := locationMap[file.Location]
		if !declared {
			locationPath = file.Location
		}
		locationPath = normalizeLocationPath(locationPath)

		if file.Name == "" {
			result.Skipped = append(result.Skipped, SkippedFile{ID: file.ID, Path: basePrefix + locationPath, Reason: "empty file name"})
			continue
		}
		rawPath := basePrefix + locationPath + file.Name
		if file.Type != "" {
			rawPath += "." + file.Type
		}
		relativePath, err := sanitizeRelativePath(rawPath)
		if err != nil {
			result.Skipped = append(result.Skipped, SkippedFile{ID: file.ID, Path: rawPath, Reason: err.Error()})
			continue
		}
		result.Resources = append(result.Resources, model.Resource{
			RelativePath: relativePath,
			URL:          JoinURL(baseURL, relativePath, file.Hash),
			Category:     category,
			ManifestPath: manifestRelPath,
			Hash:         file.Hash,
			Type:         file.Type,
		})
	}
	return result, true, nil
}

func decodeFileCollection(data []byte) (FileCollection, bool, error) {
	var collection FileCollection
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.CharsetReader = charsetReader
	for {
		token, err := decoder.Token()
		if err != nil {
			// Breaking before the root element only counts as a malformed
			// manifest when the file announced itself as XML; anything else
			// (binary data, plain text) simply is not a filecollection.
			if errors.Is(err, io.EOF) || !hasXMLDeclaration(data) {
				return collection, false, nil
			}
			return collection, false, err
		}
		start, isStart := token.(xml.StartElement)
		if !isStart {
			continue
		}
		if start.Name.Local != "filecollection" {
			return collection, false, nil
		}
		if err := decoder.DecodeElement(&collection, &start); err != nil {
			return collection, false, err
		}
		return collection, true, nil
	}
}

func hasXMLDeclaration(data []byte) bool {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	return bytes.HasPrefix(bytes.TrimLeft(data, " \t\r\n"), []byte("<?xml"))
}

// charsetReader covers the encodings seen on manifests without pulling in
// golang.org/x/net/html/charset: UTF-8/ASCII pass through, Latin-1 is
// re-encoded as UTF-8.
func charsetReader(label string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "utf8", "us-ascii", "ascii":
		return input, nil
	case "iso-8859-1", "iso8859-1", "latin1", "latin-1", "l1":
		data, err := io.ReadAll(input)
		if err != nil {
			return nil, err
		}
		out := make([]byte, 0, len(data))
		for _, b := range data {
			out = utf8.AppendRune(out, rune(b))
		}
		return bytes.NewReader(out), nil
	default:
		return nil, fmt.Errorf("unsupported charset %q", label)
	}
}

func normalizeLocationPath(locationPath string) string {
	locationPath = filepathToSlash(locationPath)
	if locationPath != "" && !strings.HasSuffix(locationPath, "/") {
		locationPath += "/"
	}
	return locationPath
}

// sanitizeRelativePath turns a server-supplied path into a clean,
// slash-separated path that stays inside the output directory.
func sanitizeRelativePath(raw string) (string, error) {
	slashed := filepathToSlash(raw)
	if strings.Contains(slashed, ":") {
		return "", errors.New("path contains ':'")
	}
	if strings.ContainsRune(slashed, 0) {
		return "", errors.New("path contains NUL")
	}
	if strings.HasPrefix(slashed, "/") {
		return "", errors.New("path is absolute")
	}
	cleaned := path.Clean(slashed)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("path escapes the output directory")
	}
	if !fs.ValidPath(cleaned) {
		return "", errors.New("path is not a valid relative path")
	}
	return cleaned, nil
}

func collectionBasePrefix(manifestRelPath string) string {
	dir := path.Dir(filepathToSlash(manifestRelPath))
	if path.Base(dir) == "xml" {
		dir = path.Dir(dir)
	}
	if dir == "." {
		return ""
	}
	return strings.TrimSuffix(dir, "/") + "/"
}

func filepathToSlash(in string) string {
	return strings.ReplaceAll(in, "\\", "/")
}

// JoinURL appends relativePath to baseURL, escaping each path segment, and
// adds the "__cv" cache-buster query parameter when hash is non-empty.
// Backslashes in relativePath are treated as separators.
func JoinURL(baseURL, relativePath, hash string) string {
	segments := strings.Split(strings.TrimLeft(filepathToSlash(relativePath), "/"), "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	joined := strings.TrimRight(baseURL, "/") + "/" + strings.Join(segments, "/")
	if hash != "" {
		joined += "?" + url.Values{"__cv": {hash}}.Encode()
	}
	return joined
}
