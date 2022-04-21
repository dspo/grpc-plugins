package genopenapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/dspo/grpc-plugins/internal/descriptor"
	gen "github.com/dspo/grpc-plugins/internal/generator"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/golang/glog"
	anypb "github.com/golang/protobuf/ptypes/any"
	openapi_options "github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-openapiv2/options"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"

	//nolint:staticcheck // Known issue, will be replaced when possible
	legacydescriptor "github.com/golang/protobuf/descriptor"
)

var (
	errNoTargetService = errors.New("no target service defined in the file")
)

type generator struct {
	reg    *descriptor.Registry
	format Format
}

type wrapper struct {
	fileName string
	swagger  *openapi3.T
}

type GeneratorOptions struct {
	Registry       *descriptor.Registry
	RecursiveDepth int
}

// New returns a new generator which generates grpc gateway files.
func New(reg *descriptor.Registry, format Format) gen.Generator {
	return &generator{
		reg:    reg,
		format: format,
	}
}

// Merge a lot of OpenAPI file (wrapper) to single one OpenAPI file
func mergeTargetFile(targets []*wrapper, mergeFileName string) *wrapper {
	var mergedTarget *wrapper
	for _, f := range targets {
		if mergedTarget == nil {
			mergedTarget = &wrapper{
				fileName: mergeFileName,
				swagger:  f.swagger,
			}
		} else {
			for k, v := range f.swagger.Components.Schemas {
				mergedTarget.swagger.Components.Schemas[k] = v
			}
			for k, v := range f.swagger.Paths {
				mergedTarget.swagger.Paths[k] = v
			}
			mergedTarget.swagger.Security = append(mergedTarget.swagger.Security, f.swagger.Security...)
		}
	}
	return mergedTarget
}

func extensionMarshalJSON(so interface{}, extensions []extension) ([]byte, error) {
	// To append arbitrary keys to the struct we'll render into json,
	// we're creating another struct that embeds the original one, and
	// its extra fields:
	//
	// The struct will look like
	// struct {
	//   *openapiCore
	//   XGrpcGatewayFoo json.RawMessage `json:"x-grpc-gateway-foo"`
	//   XGrpcGatewayBar json.RawMessage `json:"x-grpc-gateway-bar"`
	// }
	// and thus render into what we want -- the JSON of openapiCore with the
	// extensions appended.
	fields := []reflect.StructField{
		{ // embedded
			Name:      "Embedded",
			Type:      reflect.TypeOf(so),
			Anonymous: true,
		},
	}
	for _, ext := range extensions {
		fields = append(fields, reflect.StructField{
			Name: fieldName(ext.key),
			Type: reflect.TypeOf(ext.value),
			Tag:  reflect.StructTag(fmt.Sprintf("json:\"%s\"", ext.key)),
		})
	}

	t := reflect.StructOf(fields)
	s := reflect.New(t).Elem()
	s.Field(0).Set(reflect.ValueOf(so))
	for _, ext := range extensions {
		s.FieldByName(fieldName(ext.key)).Set(reflect.ValueOf(ext.value))
	}
	return json.Marshal(s.Interface())
}

// encodeOpenAPI converts OpenAPI file obj to pluginpb.CodeGeneratorResponse_File
func encodeOpenAPI(file *wrapper, format Format) (*descriptor.ResponseFile, error) {
	var contentBuf bytes.Buffer
	enc, err := format.NewEncoder(&contentBuf)
	if err != nil {
		return nil, err
	}

	if err := enc.Encode(*file.swagger); err != nil {
		return nil, err
	}

	name := file.fileName
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	output := fmt.Sprintf("%s.oas3."+string(format), base)
	return &descriptor.ResponseFile{
		CodeGeneratorResponse_File: &pluginpb.CodeGeneratorResponse_File{
			Name:    proto.String(output),
			Content: proto.String(contentBuf.String()),
		},
	}, nil
}

func (g *generator) Generate(targets []*descriptor.File) ([]*descriptor.ResponseFile, error) {
	var files []*descriptor.ResponseFile
	if g.reg.IsAllowMerge() {
		var mergedTarget *descriptor.File
		// try to find proto leader
		for _, f := range targets {
			if proto.HasExtension(f.Options, openapi_options.E_Openapiv2Swagger) {
				mergedTarget = f
				break
			}
		}
		// merge protos to leader
		for _, f := range targets {
			if mergedTarget == nil {
				mergedTarget = f
			} else if mergedTarget != f {
				mergedTarget.Enums = append(mergedTarget.Enums, f.Enums...)
				mergedTarget.Messages = append(mergedTarget.Messages, f.Messages...)
				mergedTarget.Services = append(mergedTarget.Services, f.Services...)
			}
		}

		targets = nil
		targets = append(targets, mergedTarget)
	}

	var openapis []*wrapper
	for _, file := range targets {
		glog.V(1).Infof("Processing %s", file.GetName())
		swagger, err := applyTemplate(param{File: file, reg: g.reg})
		if err == errNoTargetService {
			glog.V(1).Infof("%s: %v", file.GetName(), err)
			continue
		}
		if err != nil {
			return nil, err
		}
		openapis = append(openapis, &wrapper{
			fileName: file.GetName(),
			swagger:  swagger,
		})
	}

	if g.reg.IsAllowMerge() {
		targetOpenAPI := mergeTargetFile(openapis, g.reg.GetMergeFileName())
		f, err := encodeOpenAPI(targetOpenAPI, g.format)
		if err != nil {
			return nil, fmt.Errorf("failed to encode OpenAPI for %s: %s", g.reg.GetMergeFileName(), err)
		}
		files = append(files, f)
		glog.V(1).Infof("New OpenAPI file will emit")
	} else {
		for _, file := range openapis {
			f, err := encodeOpenAPI(file, g.format)
			if err != nil {
				return nil, fmt.Errorf("failed to encode OpenAPI for %s: %s", file.fileName, err)
			}
			files = append(files, f)
			glog.V(1).Infof("New OpenAPI file will emit")
		}
	}
	return files, nil
}

// AddErrorDefs Adds google.rpc.Status and google.protobuf.Any
// to registry (used for error-related API responses)
func AddErrorDefs(reg *descriptor.Registry) error {
	// load internal protos
	any, _ := legacydescriptor.MessageDescriptorProto(&anypb.Any{})
	any.SourceCodeInfo = new(descriptorpb.SourceCodeInfo)
	status, _ := legacydescriptor.MessageDescriptorProto(&statuspb.Status{})
	status.SourceCodeInfo = new(descriptorpb.SourceCodeInfo)
	// TODO(johanbrandhorst): Use new conversion later when possible
	// any := protodesc.ToFileDescriptorProto((&anypb.Any{}).ProtoReflect().Descriptor().ParentFile())
	// status := protodesc.ToFileDescriptorProto((&statuspb.Status{}).ProtoReflect().Descriptor().ParentFile())
	return reg.Load(&pluginpb.CodeGeneratorRequest{
		ProtoFile: []*descriptorpb.FileDescriptorProto{
			any,
			status,
		},
	})
}
