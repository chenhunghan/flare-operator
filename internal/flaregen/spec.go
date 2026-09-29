package flaregen

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// LoadSpec reads an OpenAPI document (optionally gzipped). It does not validate
// the document: the pinned Cloudflare spec is checked by make spec-check.
func LoadSpec(path string) (*openapi3.T, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return ParseSpec(data)
}

// ParseSpec parses an OpenAPI document and resolves its references.
func ParseSpec(data []byte) (*openapi3.T, error) {
	doc, err := openapi3.NewLoader().LoadFromData(data)
	if err != nil {
		return nil, fmt.Errorf("load spec: %w", err)
	}
	return doc, nil
}
