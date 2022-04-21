package genopenapi

import (
	"encoding/json"
	"fmt"

	"github.com/dspo/grpc-plugins/internal/descriptor"
	"github.com/getkin/kin-openapi/openapi3"
)

type param struct {
	*descriptor.File
	reg *descriptor.Registry
}

type extension struct {
	key   string          `json:"-" yaml:"-"`
	value json.RawMessage `json:"-" yaml:"-"`
}

type RawExample json.RawMessage

func (m RawExample) MarshalJSON() ([]byte, error) {
	return (json.RawMessage)(m).MarshalJSON()
}

func (m *RawExample) UnmarshalJSON(data []byte) error {
	return (*json.RawMessage)(m).UnmarshalJSON(data)
}

// MarshalYAML implements yaml.Marshaler interface.
//
// It converts RawExample to one of yaml-supported types and returns it.
//
// From yaml.Marshaler docs: The Marshaler interface may be implemented
// by types to customize their behavior when being marshaled into a YAML
// document. The returned value is marshaled in place of the original
// value implementing Marshaler.
func (e RawExample) MarshalYAML() (interface{}, error) {
	// From docs, json.Unmarshal will store one of next types to data:
	// - bool, for JSON booleans;
	// - float64, for JSON numbers;
	// - string, for JSON strings;
	// - []interface{}, for JSON arrays;
	// - map[string]interface{}, for JSON objects;
	// - nil for JSON null.
	var data interface{}
	if err := json.Unmarshal(e, &data); err != nil {
		return nil, err
	}

	return data, nil
}

func setRefFromFQN(schemaRef *openapi3.SchemaRef, ref string, reg *descriptor.Registry) error {
	name, ok := fullyQualifiedNameToOpenAPIName(ref, reg)
	if !ok {
		return fmt.Errorf("setRefFromFQN: can't resolve OpenAPI name from '%v'", ref)
	}
	schemaRef.Value = nil
	schemaRef.Ref = refXPath(name)
	return nil
}

type keyVal struct {
	Key   string
	Value interface{}
}

// Internal type mapping from FQMN to descriptor.Message. Used as a set by the
// findServiceMessages function.
type messageMap map[string]*descriptor.Message

// Internal type mapping from FQEN to descriptor.Enum. Used as a set by the
// findServiceMessages function.
type enumMap map[string]*descriptor.Enum

// Internal type to store used references.
type refMap map[string]struct{}
