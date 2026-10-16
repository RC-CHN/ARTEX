package resourcelib

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const DocumentName = "RESOURCE.md"
const MaxDocumentBytes = 64 << 10

type DocumentMeta struct {
	Name        string   `yaml:"name" json:"name"`
	Description string   `yaml:"description" json:"description"`
	Tags        []string `yaml:"tags,omitempty" json:"tags"`
}

// Document is the versioned entry point, following the same progressive loading
// pattern as a Skill, but using a separate registry and invocation tools.
func ParseDocument(data []byte) (DocumentMeta, error) {
	var m DocumentMeta
	if len(data) > MaxDocumentBytes {
		return m, invalid("RESOURCE.md exceeds 64 KiB")
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return m, invalid("RESOURCE.md must be UTF-8 text")
	}
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return m, invalid("RESOURCE.md needs YAML frontmatter with name and description")
	}
	end := strings.Index(normalized[4:], "\n---\n")
	if end < 0 {
		return m, invalid("RESOURCE.md frontmatter is not closed")
	}
	if err := yaml.Unmarshal([]byte(normalized[4:4+end]), &m); err != nil {
		return m, invalid("RESOURCE.md frontmatter must contain valid name, description and tags")
	}
	m.Name = strings.TrimSpace(m.Name)
	m.Description = strings.TrimSpace(m.Description)
	r := Resource{Name: m.Name, Description: m.Description, Tags: m.Tags, Kind: "file", AccessMode: "global"}
	if m.Description == "" {
		return m, invalid("RESOURCE.md description is required")
	}
	if err := ValidateResource(&r); err != nil {
		return m, err
	}
	m.Tags = r.Tags
	return m, nil
}
func initialDocument(r Resource) []byte {
	description := r.Description
	if description == "" {
		description = r.Name
	}
	meta, _ := yaml.Marshal(DocumentMeta{Name: r.Name, Description: description, Tags: r.Tags})
	return []byte("---\n" + string(meta) + "---\n\n# " + r.Name + "\n\n" + description + "\n")
}
func (s *Store) ReadDocument(ctx context.Context, m Manifest) (string, error) {
	for _, f := range m.Files {
		if f.Path != DocumentName {
			continue
		}
		if f.Size > MaxDocumentBytes {
			return "", ErrLimit
		}
		root, e := os.OpenRoot(s.Root)
		if e != nil {
			return "", e
		}
		defer root.Close()
		file, e := root.Open(blobName(f))
		if e != nil {
			return "", e
		}
		defer file.Close()
		data, e := io.ReadAll(io.LimitReader(&contextReader{ctx, file}, MaxDocumentBytes+1))
		if e != nil {
			return "", e
		}
		sum := sha256.Sum256(data)
		if int64(len(data)) != f.Size || hex.EncodeToString(sum[:]) != f.Digest {
			return "", invalid("RESOURCE.md checksum mismatch")
		}
		if _, e = ParseDocument(data); e != nil {
			return "", e
		}
		return string(data), nil
	}
	return "", invalid("RESOURCE.md is required; add the resource entry document before publishing")
}
