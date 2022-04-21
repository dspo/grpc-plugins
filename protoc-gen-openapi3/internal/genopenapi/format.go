package genopenapi

import (
	"encoding/json"
	"io"
	"strings"

	"sigs.k8s.io/yaml"
)

type Format string

const (
	FormatJSON Format = "json"
	FormatYAML Format = "yaml"
	FormatYml  Format = "yml"
)

type ContentEncoder interface {
	Encode(v interface{}) (err error)
}

// deprecated:
func (f Format) Validate() error {
	return nil
}

func (f Format) LowerString() string {
	return strings.ToLower(string(f))
}

func (f Format) NewEncoder(w io.Writer) (ContentEncoder, error) {
	switch f.LowerString() {
	case FormatJSON.LowerString():
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")

		return enc, nil
	default:
		return &yamlEncoder{w: w}, nil
	}
}

type yamlEncoder struct {
	w io.Writer
}

func (y *yamlEncoder) Encode(v interface{}) error {
	data, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	_, err = y.w.Write(data)
	return err
}
