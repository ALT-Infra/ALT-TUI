package profile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

type Document struct {
	Profile Profile
	Source  []byte
	Digest  string
}

func LoadFile(path string) (*Document, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read team profile: %w", err)
	}
	return Parse(source)
}

func Parse(source []byte) (*Document, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)

	var value Profile
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode team profile: %w", err)
	}
	if err := ensureSingleDocument(decoder); err != nil {
		return nil, err
	}
	applyDefaults(&value)

	sum := sha256.Sum256(source)
	return &Document{
		Profile: value,
		Source:  append([]byte(nil), source...),
		Digest:  hex.EncodeToString(sum[:]),
	}, nil
}

func FromValue(value Profile) (*Document, error) {
	source, err := yaml.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode team profile: %w", err)
	}
	return Parse(source)
}

func ensureSingleDocument(decoder *yaml.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if err == nil {
		return fmt.Errorf("decode team profile: multiple YAML documents are not allowed")
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return fmt.Errorf("decode team profile: %w", err)
}

func applyDefaults(p *Profile) {
	if p.Schema == 0 {
		p.Schema = CurrentSchema
	}
}
