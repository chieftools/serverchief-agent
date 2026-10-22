// Package inventory turns collector payloads into hashed datasets, builds
// the upload envelope and tracks which hashes the backend already has.
package inventory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Normalizer is implemented by payloads with lists whose order carries no
// meaning. Normalize sorts them in place so equal hosts hash equally.
type Normalizer interface {
	Normalize()
}

// Canonical encodes value as canonical JSON: object keys sorted bytewise,
// no insignificant whitespace, no HTML escaping and numbers kept exactly as
// encoding/json produced them. Struct field order and map iteration order
// therefore never affect the result. Values implementing Normalizer are
// normalized first.
func Canonical(value any) ([]byte, error) {
	if normalizer, ok := value.(Normalizer); ok {
		normalizer.Normalize()
	}

	var initial bytes.Buffer
	encoder := json.NewEncoder(&initial)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("encode payload: %w", err)
	}

	decoder := json.NewDecoder(&initial)
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}

	var output bytes.Buffer
	if err := writeCanonical(&output, generic); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func writeCanonical(writer *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		writer.WriteString("null")
	case bool:
		if typed {
			writer.WriteString("true")
		} else {
			writer.WriteString("false")
		}
	case json.Number:
		writer.WriteString(typed.String())
	case string:
		return writeString(writer, typed)
	case []any:
		writer.WriteByte('[')
		for index, element := range typed {
			if index > 0 {
				writer.WriteByte(',')
			}
			if err := writeCanonical(writer, element); err != nil {
				return err
			}
		}
		writer.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		slices.SortFunc(keys, strings.Compare)
		writer.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				writer.WriteByte(',')
			}
			if err := writeString(writer, key); err != nil {
				return err
			}
			writer.WriteByte(':')
			if err := writeCanonical(writer, typed[key]); err != nil {
				return err
			}
		}
		writer.WriteByte('}')
	default:
		return fmt.Errorf("unexpected JSON value %T", value)
	}
	return nil
}

func writeString(writer *bytes.Buffer, value string) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return err
	}
	writer.Write(bytes.TrimSuffix(buffer.Bytes(), []byte("\n")))
	return nil
}

// Hash returns the lowercase hex SHA-256 of canonical JSON.
func Hash(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// Dataset is a collected payload in canonical form.
type Dataset struct {
	Name      string
	Hash      string
	Canonical json.RawMessage
}

// NewDataset encodes and hashes a payload.
func NewDataset(name string, payload any) (Dataset, error) {
	canonical, err := Canonical(payload)
	if err != nil {
		return Dataset{}, fmt.Errorf("dataset %s: %w", name, err)
	}
	return Dataset{Name: name, Hash: Hash(canonical), Canonical: canonical}, nil
}

// HashValue is Hash(Canonical(value)).
func HashValue(value any) (string, error) {
	canonical, err := Canonical(value)
	if err != nil {
		return "", err
	}
	return Hash(canonical), nil
}
