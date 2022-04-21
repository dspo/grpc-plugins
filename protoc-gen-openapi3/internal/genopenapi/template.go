package genopenapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/dspo/grpc-plugins/internal/casing"
	"github.com/dspo/grpc-plugins/internal/descriptor"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/golang/glog"
	openapi_options "github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-openapiv2/options"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/genproto/googleapis/api/visibility"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/structpb"
)

//  The OpenAPI specification does not allow for more than one endpoint with the same HTTP method and path.
//  This prevents multiple gRPC service methods from sharing the same stripped version of the path and method.
//  For example: `GET /v1/{name=organizations/*}/roles` and `GET /v1/{name=users/*}/roles` both get stripped to `GET /v1/{name}/roles`.
//  We must make the URL unique by adding a suffix and an incrementing index to each path parameter
//  to differentiate the endpoints.
//  Since path parameter names do not affect the request contents (i.e. they're replaced in the path)
//  this will be hidden from the real grpc gateway consumer.
const pathParamUniqueSuffixDeliminator = "_"

const paragraphDeliminator = "\n\n"

// wktSchemas are the schemas of well-known-types.
// The schemas must match with the behavior of the JSON unmarshaler in
// https://github.com/protocolbuffers/protobuf-go/blob/v1.25.0/encoding/protojson/well_known_types.go
var wktSchemas = map[string]openapi3.SchemaRef{
	".google.protobuf.FieldMask": {Value: &openapi3.Schema{
		Type: "string",
	}},
	".google.protobuf.Timestamp": {Value: &openapi3.Schema{
		Type:   "string",
		Format: "date-time",
	}},
	".google.protobuf.Duration": {Value: &openapi3.Schema{
		Type: "string",
	}},
	".google.protobuf.StringValue": {Value: &openapi3.Schema{
		Type: "string",
	}},
	".google.protobuf.BytesValue": {Value: &openapi3.Schema{
		Type:   "string",
		Format: "byte",
	}},
	".google.protobuf.Int32Value": {Value: &openapi3.Schema{
		Type:   "integer",
		Format: "int32",
	}},
	".google.protobuf.UInt32Value": {Value: &openapi3.Schema{
		Type:   "integer",
		Format: "int64",
	}},
	".google.protobuf.Int64Value": {Value: &openapi3.Schema{
		Type:   "string",
		Format: "int64",
	}},
	".google.protobuf.UInt64Value": {Value: &openapi3.Schema{
		Type:   "string",
		Format: "uint64",
	}},
	".google.protobuf.FloatValue": {Value: &openapi3.Schema{
		Type:   "number",
		Format: "float",
	}},
	".google.protobuf.DoubleValue": {Value: &openapi3.Schema{
		Type:   "number",
		Format: "double",
	}},
	".google.protobuf.BoolValue": {Value: &openapi3.Schema{
		Type: "boolean",
	}},
	".google.protobuf.Empty": {Value: &openapi3.Schema{}},
	".google.protobuf.Struct": {Value: &openapi3.Schema{
		Type: "object",
	}},
	".google.protobuf.Value": {Value: &openapi3.Schema{
		Type: "object",
	}},
	".google.protobuf.ListValue": {Value: &openapi3.Schema{
		Type: "array",
		Items: &openapi3.SchemaRef{Value: &openapi3.Schema{
			Type: "object",
		}},
	}},
	".google.protobuf.NullValue": {Value: &openapi3.Schema{
		Type: "string",
	}},
}

func listEnumNames(reg *descriptor.Registry, enum *descriptor.Enum) (names []interface{}) {
	for _, value := range enum.GetValue() {
		if !isVisible(getEnumValueVisibilityOption(value), reg) {
			continue
		}
		if reg.GetOmitEnumDefaultValue() && value.GetNumber() == 0 {
			continue
		}
		names = append(names, value.GetName())
	}
	return names
}

func listEnumNumbers(reg *descriptor.Registry, enum *descriptor.Enum) (numbers []interface{}) {
	for _, value := range enum.GetValue() {
		if reg.GetOmitEnumDefaultValue() && value.GetNumber() == 0 {
			continue
		}
		if !isVisible(getEnumValueVisibilityOption(value), reg) {
			continue
		}
		numbers = append(numbers, strconv.Itoa(int(value.GetNumber())))
	}
	return
}

func getEnumDefault(reg *descriptor.Registry, enum *descriptor.Enum) string {
	if !reg.GetOmitEnumDefaultValue() {
		for _, value := range enum.GetValue() {
			if value.GetNumber() == 0 {
				return value.GetName()
			}
		}
	}
	return ""
}

// messageToQueryParameters converts a message to a list of OpenAPI query parameters.
func messageToQueryParameters(message *descriptor.Message, reg *descriptor.Registry, pathParams []descriptor.Parameter, body *descriptor.Body) (params openapi3.Parameters, err error) {
	for _, field := range message.Fields {
		if !isVisible(getFieldVisibilityOption(field), reg) {
			continue
		}

		p, err := queryParams(message, field, "", reg, pathParams, body, reg.GetRecursiveDepth())
		if err != nil {
			return nil, err
		}
		params = append(params, p...)
	}
	return params, nil
}

// queryParams converts a field to a list of OpenAPI query parameters recursively through the use of nestedQueryParams.
func queryParams(message *descriptor.Message, field *descriptor.Field, prefix string, reg *descriptor.Registry, pathParams []descriptor.Parameter, body *descriptor.Body, recursiveCount int) (params openapi3.Parameters, err error) {
	return nestedQueryParams(message, field, prefix, reg, pathParams, body, newCycleChecker(recursiveCount))
}

type cycleChecker struct {
	m     map[string]int
	count int
}

func newCycleChecker(recursive int) *cycleChecker {
	return &cycleChecker{
		m:     make(map[string]int),
		count: recursive,
	}
}

// Check returns whether name is still within recursion
// toleration
func (c *cycleChecker) Check(name string) bool {
	count, ok := c.m[name]
	count = count + 1
	isCycle := count > c.count

	if isCycle {
		return false
	}

	// provision map entry if not available
	if !ok {
		c.m[name] = 1
		return true
	}

	c.m[name] = count

	return true
}

func (c *cycleChecker) Branch() *cycleChecker {
	copy := &cycleChecker{
		count: c.count,
		m:     map[string]int{},
	}

	for k, v := range c.m {
		copy.m[k] = v
	}

	return copy
}

// nestedQueryParams converts a field to a list of OpenAPI query parameters recursively.
// This function is a helper function for queryParams, that keeps track of cyclical message references
//  through the use of
//      touched map[string]int
// If a cycle is discovered, an error is returned, as cyclical data structures are dangerous
//  in query parameters.
func nestedQueryParams(message *descriptor.Message, field *descriptor.Field, prefix string, reg *descriptor.Registry, pathParams []descriptor.Parameter, body *descriptor.Body, cycle *cycleChecker) (params openapi3.Parameters, err error) {
	// make sure the parameter is not already listed as a path parameter
	for _, pathParam := range pathParams {
		if pathParam.Target == field {
			return nil, nil
		}
	}
	// make sure the parameter is not already listed as a body parameter
	if body != nil {
		if body.FieldPath == nil {
			return nil, nil
		}
		for _, fieldPath := range body.FieldPath {
			if fieldPath.Target == field {
				return nil, nil
			}
		}
	}
	schema := schemaOfField(field, reg, nil)
	fieldType := field.GetTypeName()
	if message.File != nil {
		comments := fieldProtoComments(reg, message, field)
		if err := updateOpenAPIDataFromComments(reg, &schema, message, comments, false); err != nil {
			return nil, err
		}
	}

	isEnum := field.GetType() == descriptorpb.FieldDescriptorProto_TYPE_ENUM
	items := schema.Value.Items
	if schema.Value.Type != "" || isEnum {
		if schema.Value.Type == "object" {
			return nil, nil // TODO: currently, mapping object in query parameter is not supported
		}
		if items != nil && (items.Value.Type == "" || items.Value.Type == "object") && !isEnum {
			return nil, nil // TODO: currently, mapping object in query parameter is not supported
		}
		desc := mergeDescription(schema.Value)

		// verify if the field is required
		required := false
		for _, fieldName := range schema.Value.Required {
			if fieldName == reg.FieldName(field) {
				required = true
				break
			}
		}

		param := openapi3.ParameterRef{Value: &openapi3.Parameter{
			ExtensionProps: openapi3.ExtensionProps{},
			Name:           prefix + reg.FieldName(field),
			In:             openapi3.ParameterInQuery,
			Description:    desc,
			Required:       required,
			Schema:         schema,
		}}

		if isEnum {
			enum, err := reg.LookupEnum("", fieldType)
			if err != nil {
				return nil, fmt.Errorf("unknown enum type %s", fieldType)
			}
			if items != nil { // array
				param.Value.Schema.Value.Items = &openapi3.SchemaRef{Value: &openapi3.Schema{Type: "string", Enum: listEnumNames(reg, enum)}}
				if reg.GetEnumsAsInts() {
					param.Value.Schema.Value.Items = &openapi3.SchemaRef{Value: &openapi3.Schema{Type: "integer", Enum: listEnumNames(reg, enum)}}
				}
			} else {
				param.Value.Schema.Value.Type = "string"
				param.Value.Schema.Value.Enum = listEnumNames(reg, enum)
				param.Value.Schema.Value.Default = getEnumDefault(reg, enum)
				if reg.GetEnumsAsInts() {
					param.Value.Schema.Value.Type = "integer"
					param.Value.Schema.Value.Enum = listEnumNumbers(reg, enum)
					if !reg.GetOmitEnumDefaultValue() {
						param.Value.Schema.Value.Default = "0"
					}
				}
			}
			valueComments := enumValueProtoComments(reg, enum)
			if valueComments != "" {
				param.Value.Description = strings.TrimLeft(param.Value.Description+"\n\n "+valueComments, "\n")
			}
		}
		return openapi3.Parameters{&param}, nil
	}

	// nested type, recurse
	msg, err := reg.LookupMsg("", fieldType)
	if err != nil {
		return nil, fmt.Errorf("unknown message type %s", fieldType)
	}

	// Check for cyclical message reference:
	isOK := cycle.Check(*msg.Name)
	if !isOK {
		return nil, fmt.Errorf("exceeded recursive count (%d) for query parameter %q", cycle.count, fieldType)
	}

	// Construct a new map with the message name so a cycle further down the recursive path can be detected.
	// Do not keep anything in the original touched reference and do not pass that reference along.  This will
	// prevent clobbering adjacent records while recursing.
	touchedOut := cycle.Branch()

	for _, nestedField := range msg.Fields {
		if !isVisible(getFieldVisibilityOption(nestedField), reg) {
			continue
		}

		fieldName := reg.FieldName(field)
		p, err := nestedQueryParams(msg, nestedField, prefix+fieldName+".", reg, pathParams, body, touchedOut)
		if err != nil {
			return nil, err
		}
		params = append(params, p...)
	}
	return params, nil
}

// findServicesMessagesAndEnumerations discovers all messages and enums defined in the RPC methods of the service.
func findServicesMessagesAndEnumerations(s []*descriptor.Service, reg *descriptor.Registry, m messageMap, ms messageMap, e enumMap, refs refMap) {
	for _, svc := range s {
		for _, meth := range svc.Methods {
			// Request may be fully included in query
			{
				swgReqName, ok := fullyQualifiedNameToOpenAPIName(meth.RequestType.FQMN(), reg)
				if !ok {
					glog.Errorf("couldn't resolve OpenAPI name for FQMN '%v'", meth.RequestType.FQMN())
					continue
				}
				if _, ok := refs[refXPath(swgReqName)]; ok {
					if !skipRenderingRef(meth.RequestType.FQMN()) {
						m[swgReqName] = meth.RequestType
					}
				}
			}

			swgRspName, ok := fullyQualifiedNameToOpenAPIName(meth.ResponseType.FQMN(), reg)
			if !ok && !skipRenderingRef(meth.ResponseType.FQMN()) {
				glog.Errorf("couldn't resolve OpenAPI name for FQMN '%v'", meth.ResponseType.FQMN())
				continue
			}

			findNestedMessagesAndEnumerations(meth.RequestType, reg, m, e)

			if !skipRenderingRef(meth.ResponseType.FQMN()) {
				m[swgRspName] = meth.ResponseType
			}
			findNestedMessagesAndEnumerations(meth.ResponseType, reg, m, e)
		}
	}
}

// findNestedMessagesAndEnumerations those can be generated by the services.
func findNestedMessagesAndEnumerations(message *descriptor.Message, reg *descriptor.Registry, m messageMap, e enumMap) {
	// Iterate over all the fields that
	for _, t := range message.Fields {
		if !isVisible(getFieldVisibilityOption(t), reg) {
			continue
		}

		fieldType := t.GetTypeName()
		// If the type is an empty string then it is a proto primitive
		if fieldType != "" {
			if _, ok := m[fieldType]; !ok {
				msg, err := reg.LookupMsg("", fieldType)
				if err != nil {
					enum, err := reg.LookupEnum("", fieldType)
					if err != nil {
						panic(err)
					}
					e[fieldType] = enum
					continue
				}
				m[fieldType] = msg
				findNestedMessagesAndEnumerations(msg, reg, m, e)
			}
		}
	}
}

func skipRenderingRef(refName string) bool {
	_, ok := wktSchemas[refName]
	return ok
}

func renderMessageAsDefinition(msg *descriptor.Message, reg *descriptor.Registry, customRefs refMap, pathParams []descriptor.Parameter) (*openapi3.SchemaRef, error) {
	schema := &openapi3.Schema{Type: "object"}
	schemaRef := &openapi3.SchemaRef{Value: schema}

	msgComments := protoComments(reg, msg.File, msg.Outers, "MessageType", int32(msg.Index))
	if err := updateOpenAPIDataFromComments(reg, &schema, msg, msgComments, false); err != nil {
		return schemaRef, err
	}

	// todo: if options in proto

	schema.Required = filterOutExcludedFields(schema.Required, pathParams)

	for _, f := range msg.Fields {
		if !isVisible(getFieldVisibilityOption(f), reg) {
			continue
		}

		if shouldExcludeField(f.GetName(), pathParams) {
			continue
		}
		subPathParams := subPathParams(f.GetName(), pathParams)
		fieldSchema, err := renderFieldAsDefinition(f, reg, customRefs, subPathParams)
		if err != nil {
			return schemaRef, err
		}
		comments := fieldProtoComments(reg, msg, f)
		if err := updateOpenAPIDataFromComments(reg, &fieldSchema, f, comments, false); err != nil {
			return schemaRef, err
		}

		if requiredIdx := find(schema.Required, *f.Name); requiredIdx != -1 && reg.GetUseJSONNamesForFields() {
			schema.Required[requiredIdx] = f.GetJsonName()
		}

		if fieldSchema.Value != nil && fieldSchema.Value.Required != nil {
			schema.Required = append(schema.Required, fieldSchema.Value.Required...)
		}

		key := reg.FieldName(f)
		if schema.Properties == nil {
			schema.Properties = make(openapi3.Schemas)
		}
		schema.Properties[key] = fieldSchema
	}

	if msg.FQMN() == ".google.protobuf.Any" {
		transformAnyForJSON(schema, reg.GetUseJSONNamesForFields())
	}

	return schemaRef, nil
}

func renderFieldAsDefinition(f *descriptor.Field, reg *descriptor.Registry, refs refMap, pathParams []descriptor.Parameter) (*openapi3.SchemaRef, error) {
	if len(pathParams) == 0 {
		return schemaOfField(f, reg, refs), nil
	}
	location := ""
	if ix := strings.LastIndex(f.Message.FQMN(), "."); ix > 0 {
		location = f.Message.FQMN()[0:ix]
	}
	msg, err := reg.LookupMsg(location, f.GetTypeName())
	if err != nil {
		return openapi3.NewSchemaRef("", openapi3.NewSchema()), err
	}
	schema, err := renderMessageAsDefinition(msg, reg, refs, pathParams)
	if err != nil {
		return openapi3.NewSchemaRef("", openapi3.NewSchema()), err
	}
	comments := fieldProtoComments(reg, f.Message, f)
	if len(comments) > 0 {
		// Use title and description from field instead of nested message if present.
		paragraphs := strings.Split(comments, paragraphDeliminator)
		if schema.Value != nil {
			schema.Value.Title = strings.TrimSpace(paragraphs[0])
			schema.Value.Description = strings.TrimSpace(strings.Join(paragraphs[1:], paragraphDeliminator))
		}
	}
	return schema, nil
}

// transformAnyForJSON should be called when the schema object represents a google.protobuf.Any, and will replace the
// Properties slice with a single value for '@type'. We mutate the incorrectly named field so that we inherit the same
// documentation as specified on the original field in the protobuf descriptors.
func transformAnyForJSON(schema *openapi3.Schema, useJSONNames bool) {
	var typeFieldName string
	if useJSONNames {
		typeFieldName = "typeUrl"
	} else {
		typeFieldName = "type_url"
	}

	for name, property := range schema.Properties {
		if name == typeFieldName {
			schema.AdditionalProperties = &openapi3.SchemaRef{Value: &openapi3.Schema{}}
			schema.Properties = openapi3.Schemas{
				"@type": property,
			}
			break
		}
	}
}

func renderMessagesAsDefinition(messages messageMap, d openapi3.Schemas, reg *descriptor.Registry, customRefs refMap, pathParams []descriptor.Parameter) error {
	for name, msg := range messages {
		swgName, ok := fullyQualifiedNameToOpenAPIName(msg.FQMN(), reg)
		if !ok {
			return fmt.Errorf("can't resolve OpenAPI name from '%v'", msg.FQMN())
		}
		if skipRenderingRef(name) {
			continue
		}

		if opt := msg.GetOptions(); opt != nil && opt.MapEntry != nil && *opt.MapEntry {
			continue
		}
		var err error
		d[swgName], err = renderMessageAsDefinition(msg, reg, customRefs, pathParams)
		if err != nil {
			return err
		}
	}
	return nil
}

// isVisible checks if a field/RPC is visible based on the visibility restriction
// combined with the `visibility_restriction_selectors`.
// Elements with an overlap on `visibility_restriction_selectors` are visible, those without are not visible.
// Elements without `google.api.VisibilityRule` annotations entirely are always visible.
func isVisible(r *visibility.VisibilityRule, reg *descriptor.Registry) bool {
	if r == nil {
		return true
	}

	restrictions := strings.Split(strings.TrimSpace(r.Restriction), ",")
	// No restrictions results in the element always being visible
	if len(restrictions) == 0 {
		return true
	}

	for _, restriction := range restrictions {
		if reg.GetVisibilityRestrictionSelectors()[strings.TrimSpace(restriction)] {
			return true
		}
	}

	return false
}

func shouldExcludeField(name string, excluded []descriptor.Parameter) bool {
	for _, p := range excluded {
		if len(p.FieldPath) == 1 && name == p.FieldPath[0].Name {
			return true
		}
	}
	return false
}
func filterOutExcludedFields(fields []string, excluded []descriptor.Parameter) []string {
	var filtered []string
	for _, f := range fields {
		if !shouldExcludeField(f, excluded) {
			filtered = append(filtered, f)
		}
	}
	return filtered
}

// schemaOfField returns a OpenAPI Schema Object for a protobuf field.
func schemaOfField(f *descriptor.Field, reg *descriptor.Registry, refs refMap) *openapi3.SchemaRef {
	const (
		singular = 0
		array    = 1
		object   = 2
	)
	var (
		core      *openapi3.SchemaRef
		aggregate int
	)

	fd := f.FieldDescriptorProto
	location := ""
	if ix := strings.LastIndex(f.Message.FQMN(), "."); ix > 0 {
		location = f.Message.FQMN()[0:ix]
	}
	if m, err := reg.LookupMsg(location, f.GetTypeName()); err == nil {
		if opt := m.GetOptions(); opt != nil && opt.MapEntry != nil && *opt.MapEntry {
			fd = m.GetField()[1]
			aggregate = object
		}
	}
	if fd.GetLabel() == descriptorpb.FieldDescriptorProto_LABEL_REPEATED {
		aggregate = array
	}

	var props = make(openapi3.Schemas)

	switch ft := fd.GetType(); ft {
	case descriptorpb.FieldDescriptorProto_TYPE_ENUM,
		descriptorpb.FieldDescriptorProto_TYPE_MESSAGE,
		descriptorpb.FieldDescriptorProto_TYPE_GROUP:
		if wktSchema, ok := wktSchemas[fd.GetTypeName()]; ok {
			core = &wktSchema
			if fd.GetTypeName() == ".google.protobuf.Empty" {
				props = make(openapi3.Schemas)
			}
		} else {
			swgRef, ok := fullyQualifiedNameToOpenAPIName(fd.GetTypeName(), reg)
			if !ok {
				panic(fmt.Sprintf("can't resolve OpenAPI ref from typename '%v'", fd.GetTypeName()))
			}
			core = &openapi3.SchemaRef{Ref: refXPath(swgRef)}
			if refs != nil {
				refs[fd.GetTypeName()] = struct{}{}
			}
		}
	default:
		ftype, format, ok := primitiveSchema(ft)
		if ok {
			core = &openapi3.SchemaRef{Value: &openapi3.Schema{Type: ftype, Format: format}}
		} else {
			core = &openapi3.SchemaRef{Value: &openapi3.Schema{Type: ft.String(), Format: "UNKNOWN"}}
		}
	}

	var ret openapi3.SchemaRef

	switch aggregate {
	case array:
		ret.Value = &openapi3.Schema{Type: "array", Items: core}
	case object:
		ret.Value = &openapi3.Schema{Type: "object", AllOf: openapi3.SchemaRefs{core}, Properties: props}
	default:
		ret = *core
	}

	if reg.GetProto3OptionalNullable() && f.GetProto3Optional() {
		ret.Value.Nullable = true
	}

	return &ret
}

// primitiveSchema returns a pair of "Type" and "Format" in JSON Schema for
// the given primitive field type.
// The last return parameter is true iff the field type is actually primitive.
func primitiveSchema(t descriptorpb.FieldDescriptorProto_Type) (ftype, format string, ok bool) {
	switch t {
	case descriptorpb.FieldDescriptorProto_TYPE_DOUBLE:
		return "number", "double", true
	case descriptorpb.FieldDescriptorProto_TYPE_FLOAT:
		return "number", "float", true
	case descriptorpb.FieldDescriptorProto_TYPE_INT64:
		return "string", "int64", true
	case descriptorpb.FieldDescriptorProto_TYPE_UINT64:
		// 64bit integer types are marshaled as string in the default JSONPb marshaler.
		// TODO(yugui) Add an option to declare 64bit integers as int64.
		//
		// NOTE: uint64 is not a predefined format of integer type in OpenAPI spec.
		// So we cannot expect that uint64 is commonly supported by OpenAPI processor.
		return "string", "uint64", true
	case descriptorpb.FieldDescriptorProto_TYPE_INT32:
		return "integer", "int32", true
	case descriptorpb.FieldDescriptorProto_TYPE_FIXED64:
		// Ditto.
		return "string", "uint64", true
	case descriptorpb.FieldDescriptorProto_TYPE_FIXED32:
		// Ditto.
		return "integer", "int64", true
	case descriptorpb.FieldDescriptorProto_TYPE_BOOL:
		// NOTE: in OpenAPI specification, format should be empty on boolean type
		return "boolean", "", true
	case descriptorpb.FieldDescriptorProto_TYPE_STRING:
		// NOTE: in OpenAPI specification, format should be empty on string type
		return "string", "", true
	case descriptorpb.FieldDescriptorProto_TYPE_BYTES:
		return "string", "byte", true
	case descriptorpb.FieldDescriptorProto_TYPE_UINT32:
		// Ditto.
		return "integer", "int64", true
	case descriptorpb.FieldDescriptorProto_TYPE_SFIXED32:
		return "integer", "int32", true
	case descriptorpb.FieldDescriptorProto_TYPE_SFIXED64:
		return "string", "int64", true
	case descriptorpb.FieldDescriptorProto_TYPE_SINT32:
		return "integer", "int32", true
	case descriptorpb.FieldDescriptorProto_TYPE_SINT64:
		return "string", "int64", true
	default:
		return "", "", false
	}
}

// renderEnumerationsAsDefinition inserts enums into the definitions object.
func renderEnumerationsAsDefinition(enums enumMap, d openapi3.Schemas, reg *descriptor.Registry) {
	for _, enum := range enums {
		swgName, ok := fullyQualifiedNameToOpenAPIName(enum.FQEN(), reg)
		if !ok {
			panic(fmt.Sprintf("can't resolve OpenAPI name from FQEN '%v'", enum.FQEN()))
		}
		enumComments := protoComments(reg, enum.File, enum.Outers, "EnumType", int32(enum.Index))

		// it may be necessary to sort the result of the GetValue function.
		enumNames := listEnumNames(reg, enum)
		defaultValue := getEnumDefault(reg, enum)
		valueComments := enumValueProtoComments(reg, enum)
		if valueComments != "" {
			enumComments = strings.TrimLeft(enumComments+"\n\n "+valueComments, "\n")
		}
		enumSchemaObject := &openapi3.SchemaRef{Value: &openapi3.Schema{
			Type:    "string",
			Enum:    enumNames,
			Default: defaultValue,
		},
		}
		if reg.GetEnumsAsInts() {
			enumSchemaObject.Value.Type = "integer"
			enumSchemaObject.Value.Format = "int32"
			enumSchemaObject.Value.Default = "0"
			enumSchemaObject.Value.Enum = listEnumNumbers(reg, enum)
		}
		if err := updateOpenAPIDataFromComments(reg, &enumSchemaObject, enum, enumComments, false); err != nil {
			panic(err)
		}

		d[swgName] = enumSchemaObject
	}
}

// Take in a FQMN or FQEN and return a OpenAPI safe version of the FQMN and
// a boolean indicating if FQMN was properly resolved.
func fullyQualifiedNameToOpenAPIName(fqn string, reg *descriptor.Registry) (string, bool) {
	registriesSeenMutex.Lock()
	defer registriesSeenMutex.Unlock()
	if mapping, present := registriesSeen[reg]; present {
		ret, ok := mapping[fqn]
		return ret, ok
	}
	mapping := resolveFullyQualifiedNameToOpenAPINames(append(reg.GetAllFQMNs(), reg.GetAllFQENs()...), reg.GetOpenAPINamingStrategy())
	registriesSeen[reg] = mapping
	ret, ok := mapping[fqn]
	return ret, ok
}

// Lookup message type by location.name and return a openapiv2-safe version
// of its FQMN.
func lookupMsgAndOpenAPIName(location, name string, reg *descriptor.Registry) (*descriptor.Message, string, error) {
	msg, err := reg.LookupMsg(location, name)
	if err != nil {
		return nil, "", err
	}
	swgName, ok := fullyQualifiedNameToOpenAPIName(msg.FQMN(), reg)
	if !ok {
		return nil, "", fmt.Errorf("can't map OpenAPI name from FQMN '%v'", msg.FQMN())
	}
	return msg, swgName, nil
}

// registriesSeen is used to memoise calls to resolveFullyQualifiedNameToOpenAPINames so
// we don't repeat it unnecessarily, since it can take some time.
var registriesSeen = map[*descriptor.Registry]map[string]string{}
var registriesSeenMutex sync.Mutex

// Take the names of every proto message and generate a unique reference for each, according to the given strategy.
func resolveFullyQualifiedNameToOpenAPINames(messages []string, namingStrategy string) map[string]string {
	strategyFn := LookupNamingStrategy(namingStrategy)
	if strategyFn == nil {
		return nil
	}
	return strategyFn(messages)
}

var canRegexp = regexp.MustCompile("{([a-zA-Z][a-zA-Z0-9_.]*)([^}]*)}")

// templateToParts will split a URL template as defined by https://github.com/googleapis/googleapis/blob/master/google/api/http.proto
// into a string slice with each part as an element of the slice for use by `partsToOpenAPIPath` and `partsToRegexpMap`.
func templateToParts(path string, reg *descriptor.Registry, fields []*descriptor.Field, msgs []*descriptor.Message) []string {
	// It seems like the right thing to do here is to just use
	// strings.Split(path, "/") but that breaks badly when you hit a url like
	// /{my_field=prefix/*}/ and end up with 2 sections representing my_field.
	// Instead do the right thing and write a small pushdown (counter) automata
	// for it.
	var parts []string
	depth := 0
	buffer := ""
	jsonBuffer := ""
	for _, char := range path {
		switch char {
		case '{':
			// Push on the stack
			depth++
			buffer += string(char)
			jsonBuffer = ""
			jsonBuffer += string(char)
		case '}':
			if depth == 0 {
				panic("Encountered } without matching { before it.")
			}
			// Pop from the stack
			depth--
			buffer += string(char)
			if reg.GetUseJSONNamesForFields() &&
				len(jsonBuffer) > 1 {
				jsonSnakeCaseName := string(jsonBuffer[1:])
				jsonCamelCaseName := string(lowerCamelCase(jsonSnakeCaseName, fields, msgs))
				prev := string(buffer[:len(buffer)-len(jsonSnakeCaseName)-2])
				buffer = strings.Join([]string{prev, "{", jsonCamelCaseName, "}"}, "")
				jsonBuffer = ""
			}
		case '/':
			if depth == 0 {
				parts = append(parts, buffer)
				buffer = ""
				// Since the stack was empty when we hit the '/' we are done with this
				// section.
				continue
			}
			buffer += string(char)
			jsonBuffer += string(char)
		default:
			buffer += string(char)
			jsonBuffer += string(char)
		}
	}

	// Now append the last element to parts
	parts = append(parts, buffer)

	return parts
}

// partsToOpenAPIPath converts each path part of the form /path/{string_value=strprefix/*} which is defined in
// https://github.com/googleapis/googleapis/blob/master/google/api/http.proto to the OpenAPI expected form /path/{string_value}.
// For example this would replace the path segment of "{foo=bar/*}" with "{foo}" or "prefix{bang=bash/**}" with "prefix{bang}".
// OpenAPI 2 only allows simple path parameters with the constraints on that parameter specified in the OpenAPI
// schema's "pattern" instead of in the path parameter itself.
func partsToOpenAPIPath(parts []string, overrides map[string]string) string {
	for index, part := range parts {
		part = canRegexp.ReplaceAllString(part, "{$1}")
		if override, ok := overrides[part]; ok {
			part = override
		}
		parts[index] = part
	}
	return strings.Join(parts, "/")
}

// partsToRegexpMap returns a map of parameter name to ECMA 262 patterns
// which is what the "pattern" field on an OpenAPI parameter expects.
// See https://swagger.io/specification/v2/ (Parameter Object) and
// https://tools.ietf.org/html/draft-fge-json-schema-validation-00#section-5.2.3.
// The expression is generated based on expressions defined by https://github.com/googleapis/googleapis/blob/master/google/api/http.proto
// "Path Template Syntax" section which allow for a "param_name=foobar/*/bang/**" style expressions inside
// the path parameter placeholders that indicate constraints on the values of those parameters.
// This function will scan the split parts of a path template for parameters and
// outputs a map of the name of the parameter to a ECMA regular expression.  See the http.proto file for descriptions
// of the supported syntax. This function will ignore any path parameters that don't contain a "=" after the
// parameter name.  For supported parameters, we assume "*" represent all characters except "/" as it's
// intended to match a single path element and we assume "**" matches any character as it's intended to match multiple
// path elements.
// For example "{name=organizations/*/roles/*}" would produce the regular expression for the "name" parameter of
// "organizations/[^/]+/roles/[^/]+" or "{bar=bing/*/bang/**}" would produce the regular expression for the "bar"
// parameter of "bing/[^/]+/bang/.+".
func partsToRegexpMap(parts []string) map[string]string {
	regExps := make(map[string]string)
	for _, part := range parts {
		if submatch := canRegexp.FindStringSubmatch(part); len(submatch) > 2 {
			if strings.HasPrefix(submatch[2], "=") { // this part matches the standard and should be made into a regular expression
				// assume the string's characters other than "**" and "*" are literals (not necessarily a good assumption 100% of the times, but it will support most use cases)
				regex := submatch[2][1:]
				regex = strings.ReplaceAll(regex, "**", ".+")   // ** implies any character including "/"
				regex = strings.ReplaceAll(regex, "*", "[^/]+") // * implies any character except "/"
				regExps[submatch[1]] = regex
			}
		}
	}
	return regExps
}

func renderServiceTags(services []*descriptor.Service, reg *descriptor.Registry) openapi3.Tags {
	var tags openapi3.Tags
	for _, svc := range services {
		if !isVisible(getServiceVisibilityOption(svc), reg) {
			continue
		}
		tagName := svc.GetName()
		if pkg := svc.File.GetPackage(); pkg != "" && reg.IsIncludePackageInTags() {
			tagName = pkg + "." + tagName
		}

		tag := openapi3.Tag{
			Name: tagName,
		}
		// todo: if options in proto
		tags = append(tags, &tag)
	}
	return tags
}

func renderServices(services []*descriptor.Service, paths openapi3.Paths, reg *descriptor.Registry, requestResponseRefs, customRefs refMap, msgs []*descriptor.Message) error {
	// Correctness of svcIdx and methIdx depends on 'services' containing the services in the same order as the 'file.Service' array.
	svcBaseIdx := 0
	var lastFile *descriptor.File = nil
	for svcIdx, svc := range services {
		if svc.File != lastFile {
			lastFile = svc.File
			svcBaseIdx = svcIdx
		}

		if !isVisible(getServiceVisibilityOption(svc), reg) {
			continue
		}

		for methIdx, meth := range svc.Methods {
			if !isVisible(getMethodVisibilityOption(meth), reg) {
				continue
			}

			for bIdx, b := range meth.Bindings {
				operationFunc := operationForMethod(b.HTTPMethod)
				// Iterate over all the OpenAPI parameters
				parameters := openapi3.NewParameters()
				// split the path template into its parts
				parts := templateToParts(b.PathTmpl.Template, reg, meth.RequestType.Fields, msgs)
				// extract any constraints specified in the path placeholders into ECMA regular expressions
				pathParamRegexpMap := partsToRegexpMap(parts)
				// Keep track of path parameter overrides
				var pathParamNames = make(map[string]string)
				for _, parameter := range b.PathParams {

					var paramType, paramFormat, desc, defaultValue string
					var enumNames []interface{}
					var items = openapi3.SchemaRef{}
					var minItems = new(int)
					var extensions = make(map[string]interface{})
					switch pt := parameter.Target.GetType(); pt {
					case descriptorpb.FieldDescriptorProto_TYPE_GROUP, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE:
						if descriptor.IsWellKnownType(parameter.Target.GetTypeName()) {
							if parameter.IsRepeated() {
								return fmt.Errorf("only primitive and enum types are allowed in repeated path parameters")
							}
							schema := schemaOfField(parameter.Target, reg, customRefs)
							if schema.Value != nil {
								desc = schema.Value.Description
								if s, ok := schema.Value.Default.(string); ok {
									defaultValue = s
								}
								extensions = schema.Value.Extensions
							}
						} else {
							return fmt.Errorf("only primitive and well-known types are allowed in path parameters")
						}
					case descriptorpb.FieldDescriptorProto_TYPE_ENUM:
						enum, err := reg.LookupEnum("", parameter.Target.GetTypeName())
						if err != nil {
							return err
						}
						paramType = "string"
						paramFormat = ""
						enumNames = listEnumNames(reg, enum)
						if reg.GetEnumsAsInts() {
							paramType = "integer"
							paramFormat = ""
							enumNames = listEnumNumbers(reg, enum)
						}
						schema := schemaOfField(parameter.Target, reg, customRefs)
						if schema.Value != nil {
							desc = schema.Value.Description
							if s, ok := schema.Value.Default.(string); ok {
								defaultValue = s
							}
							extensions = schema.Value.Extensions
						}
					default:
						var ok bool
						paramType, paramFormat, ok = primitiveSchema(pt)
						if !ok {
							return fmt.Errorf("unknown field type %v", pt)
						}

						schema := schemaOfField(parameter.Target, reg, customRefs)
						if schema.Value != nil {
							desc = schema.Value.Description
							if s, ok := schema.Value.Default.(string); ok {
								defaultValue = s
							}
							extensions = schema.Value.Extensions
						}
					}

					if parameter.IsRepeated() {
						var core = &openapi3.SchemaRef{Value: &openapi3.Schema{Type: paramType, Format: paramFormat}}
						if parameter.IsEnum() {
							core.Value.Enum = enumNames
							enumNames = nil
						}
						items = *core
						paramType = "array"
						paramFormat = ""
						minItems = new(int)
						*minItems = 1
					}

					if desc == "" {
						desc = fieldProtoComments(reg, parameter.Target.Message, parameter.Target)
					}
					parameterString := parameter.String()
					if reg.GetUseJSONNamesForFields() {
						parameterString = lowerCamelCase(parameterString, meth.RequestType.Fields, msgs)
					}
					var pattern string
					if regExp, ok := pathParamRegexpMap[parameterString]; ok {
						pattern = regExp
					}
					parameters = append(parameters, &openapi3.ParameterRef{Value: &openapi3.Parameter{
						ExtensionProps: openapi3.ExtensionProps{},
						Name:           parameterString,
						In:             openapi3.ParameterInPath,
						Description:    desc,
						Required:       true,
						Schema: &openapi3.SchemaRef{Value: &openapi3.Schema{
							ExtensionProps: openapi3.ExtensionProps{Extensions: extensions},
							Type:           paramType,
							Format:         paramFormat,
							Enum:           enumNames,
							Default:        defaultValue,
							Pattern:        pattern,
							MinItems:       uint64(*minItems),
							Items:          &items,
						}},
					}})
				}
				// Now check if there is a body parameter
				if b.Body != nil {
					// Recursively render fields as definitions as long as they contain path parameters.
					// Special case for top level body if we don't have a body field.
					var schema = &openapi3.SchemaRef{Value: openapi3.NewSchema()}
					desc := ""
					var bodyFieldName string
					if len(b.Body.FieldPath) == 0 {
						// No field for body, use type.
						bodyFieldName = "body"
						wknSchemaCore, isWkn := wktSchemas[meth.RequestType.FQMN()]
						if isWkn {
							schema = &wknSchemaCore
							// Special workaround for Empty: it's well-known type but wknSchemas only returns schema.schemaCore; but we need to set schema.Properties which is a level higher.
							if meth.RequestType.FQMN() == ".google.protobuf.Empty" {
								schema.Value = openapi3.NewSchema()
							}
						} else {
							if len(b.PathParams) == 0 {
								fullyQualifiedNameToOpenAPIName(meth.RequestType.FQMN(), reg)
								err := setRefFromFQN(schema, meth.RequestType.FQMN(), reg)
								if err != nil {
									return err
								}
							} else {
								var err error
								schema, err = renderMessageAsDefinition(meth.RequestType, reg, customRefs, b.PathParams)
								if err != nil {
									return err
								}
								if schema.Value != nil && schema.Value.Properties == nil || len(schema.Value.Properties) == 0 {
									glog.Warningf("created a body with 0 properties in the message, this might be unintended: %s", *meth.RequestType)
								}
							}
						}
					} else {
						// Body field path is limited to one path component. From google.api.HttpRule.body:
						// "NOTE: the referred field must be present at the top-level of the request message type."
						// Ref: https://github.com/googleapis/googleapis/blob/b3397f5febbf21dfc69b875ddabaf76bee765058/google/api/http.proto#L350-L352
						if len(b.Body.FieldPath) > 1 {
							return fmt.Errorf("Body of request '%s' is not a top level field: '%v'.", meth.Service.GetName(), b.Body.FieldPath)
						}
						bodyField := b.Body.FieldPath[0]
						if reg.GetUseJSONNamesForFields() {
							bodyFieldName = lowerCamelCase(bodyField.Name, meth.RequestType.Fields, msgs)
						} else {
							bodyFieldName = bodyField.Name
						}
						// Align pathParams with body field path.
						pathParams := subPathParams(bodyFieldName, b.PathParams)
						var err error
						schema, err = renderFieldAsDefinition(bodyField.Target, reg, customRefs, pathParams)
						if err != nil {
							return err
						}
						if schema.Value != nil && schema.Value.Title != "" {
							desc = mergeDescription(schema.Value)
						} else {
							desc = fieldProtoComments(reg, bodyField.Target.Message, bodyField.Target)
						}
					}

					if meth.GetClientStreaming() {
						desc += " (streaming inputs)"
					}
				}

				// add the parameters to the query string
				queryParams, err := messageToQueryParameters(meth.RequestType, reg, b.PathParams, b.Body)
				if err != nil {
					return err
				}
				parameters = append(parameters, queryParams...)

				path := partsToOpenAPIPath(parts, pathParamNames)
				pathItemObject, ok := paths[path]
				if !ok {
					pathItemObject = &openapi3.PathItem{}
				} else {
					// handle case where we have an existing mapping for the same path and method
					existingOperationObject := operationFunc(pathItemObject)
					if existingOperationObject != nil {
						var firstPathParameter *openapi3.ParameterRef
						var firstParamIndex int
						for index, param := range parameters {
							if param.Value.In == openapi3.ParameterInPath {
								firstPathParameter = param
								firstParamIndex = index
								break
							}
						}
						if firstPathParameter == nil {
							// Without a path parameter, there is nothing to vary to support multiple mappings of the same path/method.
							// Previously this did not log an error and only overwrote the mapping, we now log the error but
							// still overwrite the mapping
							glog.Errorf("Duplicate mapping for path %s %s", b.HTTPMethod, path)
						} else {
							newPathCount := 0
							var newPath string
							var newPathElement string
							// Iterate until there is not an existing operation that matches the same escaped path.
							// Most of the time this will only be a single iteration, but a large API could technically have
							// a pretty large amount of these if it used similar patterns for all its functions.
							for existingOperationObject != nil {
								newPathCount += 1
								newPathElement = firstPathParameter.Value.Name + pathParamUniqueSuffixDeliminator + strconv.Itoa(newPathCount)
								newPath = strings.ReplaceAll(path, "{"+firstPathParameter.Value.Name+"}", "{"+newPathElement+"}")
								if newPathItemObject, ok := paths[newPath]; ok {
									existingOperationObject = operationFunc(newPathItemObject)
								} else {
									existingOperationObject = nil
								}
							}
							// update the pathItemObject we are adding to with the new path
							pathItemObject = paths[newPath]
							firstPathParameter.Value.Name = newPathElement
							path = newPath
							parameters[firstParamIndex] = firstPathParameter
						}
					}
				}

				methProtoPath := protoPathIndex(reflect.TypeOf((*descriptorpb.ServiceDescriptorProto)(nil)), "Method")
				desc := "A successful response."
				var responseSchema openapi3.SchemaRef

				if b.ResponseBody == nil || len(b.ResponseBody.FieldPath) == 0 {
					responseSchema = openapi3.SchemaRef{Value: openapi3.NewSchema()}

					// Don't link to a full definition for
					// empty; it's overly verbose.
					// schema.Properties{} renders it as
					// well, without a definition
					wknSchemaCore, isWkn := wktSchemas[meth.ResponseType.FQMN()]
					if !isWkn {
						if err := setRefFromFQN(&responseSchema, meth.ResponseType.FQMN(), reg); err != nil {
							return err
						}
					} else {
						responseSchema = wknSchemaCore

						// Special workaround for Empty: it's well-known type but wknSchemas only returns schema.schemaCore; but we need to set schema.Properties which is a level higher.
						if meth.ResponseType.FQMN() == ".google.protobuf.Empty" {
							responseSchema.Value.Properties = make(map[string]*openapi3.SchemaRef)
						}
					}
				} else {
					// This is resolving the value of response_body in the google.api.HttpRule
					lastField := b.ResponseBody.FieldPath[len(b.ResponseBody.FieldPath)-1]
					responseSchema = *schemaOfField(lastField.Target, reg, customRefs)
					if responseSchema.Value != nil && responseSchema.Value.Description != "" {
						desc = responseSchema.Value.Description
					} else {
						desc = fieldProtoComments(reg, lastField.Target.Message, lastField.Target)
					}
				}
				if meth.GetServerStreaming() {
					desc += "(streaming responses)"
					responseSchema.Value.Type = "object"
					swgRef, _ := fullyQualifiedNameToOpenAPIName(meth.ResponseType.FQMN(), reg)
					responseSchema.Value.Title = "Stream result of " + swgRef

					props := openapi3.Schemas{"result": &openapi3.SchemaRef{Ref: responseSchema.Ref}}
					statusDef, hasStatus := fullyQualifiedNameToOpenAPIName(".google.rpc.Status", reg)
					if hasStatus {
						props["error"] = &openapi3.SchemaRef{Ref: refXPath(statusDef)}
					}
					responseSchema.Value.Properties = props
					responseSchema.Ref = ""
				}

				tag := svc.GetName()
				if pkg := svc.File.GetPackage(); pkg != "" && reg.IsIncludePackageInTags() {
					tag = pkg + "." + tag
				}

				operationObject := &openapi3.Operation{
					Tags:        []string{tag},
					Parameters:  parameters,
					RequestBody: nil, // todo: how go generate request body
					Responses: openapi3.Responses{"200": &openapi3.ResponseRef{Value: &openapi3.Response{
						Description: &desc,
						Content: openapi3.Content{
							"application/json": &openapi3.MediaType{Schema: &responseSchema},
						},
					}}},
				}
				if !reg.GetDisableDefaultErrors() {
					errDef, hasErrDef := fullyQualifiedNameToOpenAPIName(".google.rpc.Status", reg)
					if hasErrDef {
						// https://github.com/OAI/OpenAPI-Specification/blob/3.0.0/versions/2.0.md#responses-object
						errDesc := "An unexpected error response."
						operationObject.Responses["default"] = &openapi3.ResponseRef{Value: &openapi3.Response{
							Description: &errDesc,
							Content: openapi3.Content{
								"application/json": &openapi3.MediaType{Schema: &openapi3.SchemaRef{Ref: refXPath(errDef)}},
							},
						}}
					}
				}
				operationObject.OperationID = fmt.Sprintf("%s_%s", svc.GetName(), meth.GetName())
				if reg.GetSimpleOperationIDs() {
					operationObject.OperationID = meth.GetName()
				}
				if bIdx != 0 {
					// OperationID must be unique in an OpenAPI v2 definition.
					operationObject.OperationID += strconv.Itoa(bIdx + 1)
				}

				// Fill reference map with referenced request messages
				for _, param := range operationObject.Parameters {
					if param.Ref != "" {
						requestResponseRefs[param.Ref] = struct{}{}
					}
				}

				methComments := protoComments(reg, svc.File, nil, "Service", int32(svcIdx-svcBaseIdx), methProtoPath, int32(methIdx))
				if err := updateOpenAPIDataFromComments(reg, operationObject, meth, methComments, false); err != nil {
					panic(err)
				}

				// todo: if options in proto

				switch b.HTTPMethod {
				case http.MethodGet:
					pathItemObject.Get = operationObject
				case http.MethodPost:
					pathItemObject.Post = operationObject
				case http.MethodPut:
					pathItemObject.Put = operationObject
				case http.MethodDelete:
					pathItemObject.Delete = operationObject
				case http.MethodPatch:
					pathItemObject.Patch = operationObject
				case http.MethodConnect:
					pathItemObject.Connect = operationObject
				case http.MethodHead:
					pathItemObject.Head = operationObject
				case http.MethodOptions:
					pathItemObject.Options = operationObject
				case http.MethodTrace:
					pathItemObject.Trace = operationObject
				}
				paths[path] = pathItemObject
			}
		}
	}

	// Success! return nil on the error object
	return nil
}

func mergeDescription(schema *openapi3.Schema) string {
	desc := schema.Description
	if schema.Title != "" { // join title because title of parameter object will be ignored
		desc = strings.TrimSpace(schema.Title + paragraphDeliminator + schema.Description)
	}
	return desc
}

func operationForMethod(httpMethod string) func(item *openapi3.PathItem) *openapi3.Operation {
	switch httpMethod {
	case http.MethodGet:
		return func(obj *openapi3.PathItem) *openapi3.Operation { return obj.Get }
	case http.MethodPost:
		return func(obj *openapi3.PathItem) *openapi3.Operation { return obj.Post }
	case http.MethodPut:
		return func(obj *openapi3.PathItem) *openapi3.Operation { return obj.Put }
	case http.MethodDelete:
		return func(obj *openapi3.PathItem) *openapi3.Operation { return obj.Delete }
	case http.MethodPatch:
		return func(obj *openapi3.PathItem) *openapi3.Operation { return obj.Patch }
	case http.MethodConnect:
		return func(obj *openapi3.PathItem) *openapi3.Operation { return obj.Connect }
	case http.MethodHead:
		return func(obj *openapi3.PathItem) *openapi3.Operation { return obj.Head }
	case http.MethodOptions:
		return func(obj *openapi3.PathItem) *openapi3.Operation { return obj.Options }
	case http.MethodTrace:
		return func(obj *openapi3.PathItem) *openapi3.Operation { return obj.Trace }
	default:
		return nil
	}
}

// This function is called with a param which contains the entire definition of a method.
func applyTemplate(p param) (*openapi3.T, error) {
	// Create the basic template object. This is the object that everything is
	// defined off of.
	var s = openapi3.T{
		OpenAPI: "3.0.0",
		Components: openapi3.Components{
			Schemas:         make(openapi3.Schemas),
			Parameters:      make(openapi3.ParametersMap),
			Headers:         make(openapi3.Headers),
			RequestBodies:   make(openapi3.RequestBodies),
			Responses:       make(openapi3.Responses),
			SecuritySchemes: make(openapi3.SecuritySchemes),
			Examples:        make(openapi3.Examples),
			Links:           make(openapi3.Links),
			Callbacks:       make(openapi3.Callbacks),
		},
		Info: &openapi3.Info{
			ExtensionProps: openapi3.ExtensionProps{},
			Title:          strings.TrimSuffix(p.File.GetName(), filepath.Ext(p.File.GetName())),
			Version:        "0.0.1",
		},
		Paths:    make(openapi3.Paths),
		Security: *openapi3.NewSecurityRequirements(),
		Servers:  openapi3.Servers{&openapi3.Server{}},
	}

	// Loops through all the services and their exposed GET/POST/PUT/DELETE definitions
	// and create entries for all of them.
	// Also adds custom user specified references to second map.
	requestResponseRefs, customRefs := refMap{}, refMap{}
	if err := renderServices(p.Services, s.Paths, p.reg, requestResponseRefs, customRefs, p.Messages); err != nil {
		panic(err)
	}
	s.Tags = append(s.Tags, renderServiceTags(p.Services, p.reg)...)

	messages := messageMap{}
	streamingMessages := messageMap{}
	enums := enumMap{}

	if !p.reg.GetDisableDefaultErrors() {
		// Add the error type to the message map
		runtimeError, swgRef, err := lookupMsgAndOpenAPIName("google.rpc", "Status", p.reg)
		if err == nil {
			messages[swgRef] = runtimeError
		} else {
			// just in case there is an error looking up runtimeError
			glog.Error(err)
		}
	}

	// Find all the service's messages and enumerations that are defined (recursively)
	// and write request, response and other custom (but referenced) types out as definition objects.
	findServicesMessagesAndEnumerations(p.Services, p.reg, messages, streamingMessages, enums, requestResponseRefs)
	if err := renderMessagesAsDefinition(messages, s.Components.Schemas, p.reg, customRefs, nil); err != nil {
		return nil, err
	}
	renderEnumerationsAsDefinition(enums, s.Components.Schemas, p.reg)

	// File itself might have some comments and metadata.
	packageProtoPath := protoPathIndex(reflect.TypeOf((*descriptorpb.FileDescriptorProto)(nil)), "Package")
	packageComments := protoComments(p.reg, p.File, nil, "Package", packageProtoPath)
	if err := updateOpenAPIDataFromComments(p.reg, &s, p, packageComments, true); err != nil {
		return nil, err
	}

	// todo: if options in proto
	// There may be additional options in the OpenAPI option in the proto.

	// Finally add any references added by users that aren't
	// otherwise rendered.
	if err := addCustomRefs(s.Components.Schemas, p.reg, customRefs); err != nil {
		return nil, err
	}

	return &s, nil
}

func processExtensions(inputExts map[string]*structpb.Value) (map[string]interface{}, error) {
	var exts = make(map[string]interface{})
	for k, v := range inputExts {
		if !strings.HasPrefix(k, "x-") {
			return nil, fmt.Errorf("extension keys need to start with \"x-\": %q", k)
		}
		ext, err := (&protojson.MarshalOptions{Indent: "  "}).Marshal(v)
		if err != nil {
			return nil, err
		}
		exts[k] = json.RawMessage(ext)

	}
	return exts, nil
}

func validateHeaderTypeAndFormat(headerType string, format string) error {
	// The type of the object. The value MUST be one of "string", "number", "integer", "boolean", or "array"
	// See: https://github.com/OAI/OpenAPI-Specification/blob/3.0.0/versions/2.0.md#headerObject
	// Note: currently not implementing array as we are only implementing this in the operation response context
	switch headerType {
	// the format property is an open string-valued property, and can have any value to support documentation needs
	// primary check for format is to ensure that the number/integer formats are extensions of the specified type
	// See: https://github.com/OAI/OpenAPI-Specification/blob/3.0.0/versions/2.0.md#dataTypeFormat
	case "string":
		return nil
	case "number":
		switch format {
		case "uint",
			"uint8",
			"uint16",
			"uint32",
			"uint64",
			"int",
			"int8",
			"int16",
			"int32",
			"int64",
			"float",
			"float32",
			"float64",
			"complex64",
			"complex128",
			"double",
			"byte",
			"rune",
			"uintptr",
			"":
			return nil
		default:
			return fmt.Errorf("the provided format %q is not a valid extension of the type %q", format, headerType)
		}
	case "integer":
		switch format {
		case "uint",
			"uint8",
			"uint16",
			"uint32",
			"uint64",
			"int",
			"int8",
			"int16",
			"int32",
			"int64",
			"":
			return nil
		default:
			return fmt.Errorf("the provided format %q is not a valid extension of the type %q", format, headerType)
		}
	case "boolean":
		return nil
	}
	return fmt.Errorf("the provided header type %q is not supported", headerType)
}

func validateDefaultValueTypeAndFormat(headerType string, defaultValue string, format string) error {
	switch headerType {
	case "string":
		if !isQuotedString(defaultValue) {
			return fmt.Errorf("the provided default value %q does not match provider type %q, or is not properly quoted with escaped quotations", defaultValue, headerType)
		}
		switch format {
		case "date-time":
			unquoteTime := strings.Trim(defaultValue, `"`)
			_, err := time.Parse(time.RFC3339, unquoteTime)
			if err != nil {
				return fmt.Errorf("the provided default value %q is not a valid RFC3339 date-time string", defaultValue)
			}
		case "date":
			const (
				layoutRFC3339Date = "2006-01-02"
			)
			unquoteDate := strings.Trim(defaultValue, `"`)
			_, err := time.Parse(layoutRFC3339Date, unquoteDate)
			if err != nil {
				return fmt.Errorf("the provided default value %q is not a valid RFC3339 date-time string", defaultValue)
			}
		}
	case "number":
		err := isJSONNumber(defaultValue, headerType)
		if err != nil {
			return err
		}
	case "integer":
		switch format {
		case "int32":
			_, err := strconv.ParseInt(defaultValue, 0, 32)
			if err != nil {
				return fmt.Errorf("the provided default value %q does not match provided format %q", defaultValue, format)
			}
		case "uint32":
			_, err := strconv.ParseUint(defaultValue, 0, 32)
			if err != nil {
				return fmt.Errorf("the provided default value %q does not match provided format %q", defaultValue, format)
			}
		case "int64":
			_, err := strconv.ParseInt(defaultValue, 0, 64)
			if err != nil {
				return fmt.Errorf("the provided default value %q does not match provided format %q", defaultValue, format)
			}
		case "uint64":
			_, err := strconv.ParseUint(defaultValue, 0, 64)
			if err != nil {
				return fmt.Errorf("the provided default value %q does not match provided format %q", defaultValue, format)
			}
		default:
			_, err := strconv.ParseInt(defaultValue, 0, 64)
			if err != nil {
				return fmt.Errorf("the provided default value %q does not match provided type %q", defaultValue, headerType)
			}
		}
	case "boolean":
		if !isBool(defaultValue) {
			return fmt.Errorf("the provided default value %q does not match provider type %q", defaultValue, headerType)
		}
	}
	return nil
}

func isQuotedString(s string) bool {
	return len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"'
}

func isJSONNumber(s string, t string) error {
	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("the provided default value %q does not match provider type %q", s, t)
	}
	// Floating point values that cannot be represented as sequences of digits (such as Infinity and NaN) are not permitted.
	// See: https://tools.ietf.org/html/rfc4627#section-2.4
	if math.IsInf(val, 0) || math.IsNaN(val) {
		return fmt.Errorf("the provided number %q is not a valid JSON number", s)
	}

	return nil
}

func isBool(s string) bool {
	// Unable to use strconv.ParseBool because it returns truthy values https://golang.org/pkg/strconv/#example_ParseBool
	// per https://swagger.io/specification/v2/#data-types
	// type: boolean represents two values: true and false. Note that truthy and falsy values such as "true", "", 0 or null are not considered boolean values.
	return s == "true" || s == "false"
}

// updateOpenAPIDataFromComments updates a OpenAPI object based on a comment
// from the proto file.
//
// First paragraph of a comment is used for summary. Remaining paragraphs of
// a comment are used for description. If 'Summary' field is not present on
// the passed swaggerObject, the summary and description are joined by \n\n.
//
// If there is a field named 'Info', its 'Summary' and 'Description' fields
// will be updated instead.
//
// If there is no 'Summary', the same behavior will be attempted on 'Title',
// but only if the last character is not a period.
func updateOpenAPIDataFromComments(reg *descriptor.Registry, swaggerObject interface{}, data interface{}, comment string, isPackageObject bool) error {
	if len(comment) == 0 {
		return nil
	}

	// Checks whether the "use_go_templates" flag is set to true
	if reg.GetUseGoTemplate() {
		comment = goTemplateComments(comment, data, reg)
	}

	// Figure out what to apply changes to.
	swaggerObjectValue := reflect.ValueOf(swaggerObject)
	infoObjectValue := swaggerObjectValue.Elem().FieldByName("Info")
	if !infoObjectValue.CanSet() {
		// No such field? Apply summary and description directly to
		// passed object.
		infoObjectValue = swaggerObjectValue.Elem()
	}

	// Figure out which properties to update.
	summaryValue := infoObjectValue.FieldByName("Summary")
	descriptionValue := infoObjectValue.FieldByName("Description")
	readOnlyValue := infoObjectValue.FieldByName("ReadOnly")

	if readOnlyValue.Kind() == reflect.Bool && readOnlyValue.CanSet() && strings.Contains(comment, "Output only.") {
		readOnlyValue.Set(reflect.ValueOf(true))
	}

	usingTitle := false
	if !summaryValue.CanSet() {
		summaryValue = infoObjectValue.FieldByName("Title")
		usingTitle = true
	}

	paragraphs := strings.Split(comment, paragraphDeliminator)

	// If there is a summary (or summary-equivalent) and it's empty, use the first
	// paragraph as summary, and the rest as description.
	if summaryValue.CanSet() {
		summary := strings.TrimSpace(paragraphs[0])
		description := strings.TrimSpace(strings.Join(paragraphs[1:], paragraphDeliminator))
		if !usingTitle || (len(summary) > 0 && summary[len(summary)-1] != '.') {
			// overrides the schema value only if it's empty
			// keep the comment precedence when updating the package definition
			if summaryValue.Len() == 0 || isPackageObject {
				summaryValue.Set(reflect.ValueOf(summary))
			}
			if len(description) > 0 {
				if !descriptionValue.CanSet() {
					return fmt.Errorf("encountered object type with a summary, but no description")
				}
				// overrides the schema value only if it's empty
				// keep the comment precedence when updating the package definition
				if descriptionValue.Len() == 0 || isPackageObject {
					descriptionValue.Set(reflect.ValueOf(description))
				}
			}
			return nil
		}
	}

	// There was no summary field on the swaggerObject. Try to apply the
	// whole comment into description if the OpenAPI object description is empty.
	if descriptionValue.CanSet() {
		if descriptionValue.Len() == 0 || isPackageObject {
			descriptionValue.Set(reflect.ValueOf(strings.Join(paragraphs, paragraphDeliminator)))
		}
		return nil
	}

	return fmt.Errorf("no description nor summary property")
}

func fieldProtoComments(reg *descriptor.Registry, msg *descriptor.Message, field *descriptor.Field) string {
	protoPath := protoPathIndex(reflect.TypeOf((*descriptorpb.DescriptorProto)(nil)), "Field")
	for i, f := range msg.Fields {
		if f == field {
			return protoComments(reg, msg.File, msg.Outers, "MessageType", int32(msg.Index), protoPath, int32(i))
		}
	}
	return ""
}

func enumValueProtoComments(reg *descriptor.Registry, enum *descriptor.Enum) string {
	protoPath := protoPathIndex(reflect.TypeOf((*descriptorpb.EnumDescriptorProto)(nil)), "Value")
	var comments []string
	for idx, value := range enum.GetValue() {
		if !isVisible(getEnumValueVisibilityOption(value), reg) {
			continue
		}
		name := value.GetName()
		if reg.GetEnumsAsInts() {
			name = strconv.Itoa(int(value.GetNumber()))
		}
		str := protoComments(reg, enum.File, enum.Outers, "EnumType", int32(enum.Index), protoPath, int32(idx))
		if str != "" {
			comments = append(comments, name+": "+str)
		}
	}
	if len(comments) > 0 {
		return "- " + strings.Join(comments, "\n - ")
	}
	return ""
}

func protoComments(reg *descriptor.Registry, file *descriptor.File, outers []string, typeName string, typeIndex int32, fieldPaths ...int32) string {
	if file.SourceCodeInfo == nil {
		fmt.Fprintln(os.Stderr, "descriptor.File should not contain nil SourceCodeInfo")
		return ""
	}

	outerPaths := make([]int32, len(outers))
	for i := range outers {
		location := ""
		if file.Package != nil {
			location = file.GetPackage()
		}

		msg, err := reg.LookupMsg(location, strings.Join(outers[:i+1], "."))
		if err != nil {
			panic(err)
		}
		outerPaths[i] = int32(msg.Index)
	}

	for _, loc := range file.SourceCodeInfo.Location {
		if !isProtoPathMatches(loc.Path, outerPaths, typeName, typeIndex, fieldPaths) {
			continue
		}
		comments := ""
		if loc.LeadingComments != nil {
			comments = strings.TrimRight(*loc.LeadingComments, "\n")
			comments = strings.TrimSpace(comments)
			// TODO(ivucica): this is a hack to fix "// " being interpreted as "//".
			// perhaps we should:
			// - split by \n
			// - determine if every (but first and last) line begins with " "
			// - trim every line only if that is the case
			// - join by \n
			comments = strings.Replace(comments, "\n ", "\n", -1)
		}
		return comments
	}
	return ""
}

func goTemplateComments(comment string, data interface{}, reg *descriptor.Registry) string {
	var temp bytes.Buffer
	tpl, err := template.New("").Funcs(template.FuncMap{
		// Allows importing documentation from a file
		"import": func(name string) string {
			file, err := ioutil.ReadFile(name)
			if err != nil {
				return err.Error()
			}
			// Runs template over imported file
			return goTemplateComments(string(file), data, reg)
		},
		// Grabs title and description from a field
		"fieldcomments": func(msg *descriptor.Message, field *descriptor.Field) string {
			return strings.Replace(fieldProtoComments(reg, msg, field), "\n", "<br>", -1)
		},
	}).Parse(comment)
	if err != nil {
		// If there is an error parsing the templating insert the error as string in the comment
		// to make it easier to debug the template error
		return err.Error()
	}
	err = tpl.Execute(&temp, data)
	if err != nil {
		// If there is an error executing the templating insert the error as string in the comment
		// to make it easier to debug the error
		return err.Error()
	}
	return temp.String()
}

var messageProtoPath = protoPathIndex(reflect.TypeOf((*descriptorpb.FileDescriptorProto)(nil)), "MessageType")
var nestedProtoPath = protoPathIndex(reflect.TypeOf((*descriptorpb.DescriptorProto)(nil)), "NestedType")
var packageProtoPath = protoPathIndex(reflect.TypeOf((*descriptorpb.FileDescriptorProto)(nil)), "Package")
var serviceProtoPath = protoPathIndex(reflect.TypeOf((*descriptorpb.FileDescriptorProto)(nil)), "Service")
var methodProtoPath = protoPathIndex(reflect.TypeOf((*descriptorpb.ServiceDescriptorProto)(nil)), "Method")

func isProtoPathMatches(paths []int32, outerPaths []int32, typeName string, typeIndex int32, fieldPaths []int32) bool {
	if typeName == "Package" && typeIndex == packageProtoPath {
		// path for package comments is just [2], and all the other processing
		// is too complex for it.
		if len(paths) == 0 || typeIndex != paths[0] {
			return false
		}
		return true
	}

	if len(paths) != len(outerPaths)*2+2+len(fieldPaths) {
		return false
	}

	if typeName == "Method" {
		if paths[0] != serviceProtoPath || paths[2] != methodProtoPath {
			return false
		}
		paths = paths[2:]
	} else {
		typeNameDescriptor := reflect.TypeOf((*descriptorpb.FileDescriptorProto)(nil))

		if len(outerPaths) > 0 {
			if paths[0] != messageProtoPath || paths[1] != outerPaths[0] {
				return false
			}
			paths = paths[2:]
			outerPaths = outerPaths[1:]

			for i, v := range outerPaths {
				if paths[i*2] != nestedProtoPath || paths[i*2+1] != v {
					return false
				}
			}
			paths = paths[len(outerPaths)*2:]

			if typeName == "MessageType" {
				typeName = "NestedType"
			}
			typeNameDescriptor = reflect.TypeOf((*descriptorpb.DescriptorProto)(nil))
		}

		if paths[0] != protoPathIndex(typeNameDescriptor, typeName) || paths[1] != typeIndex {
			return false
		}
		paths = paths[2:]
	}

	for i, v := range fieldPaths {
		if paths[i] != v {
			return false
		}
	}
	return true
}

// protoPathIndex returns a path component for google.protobuf.descriptor.SourceCode_Location.
//
// Specifically, it returns an id as generated from descriptor proto which
// can be used to determine what type the id following it in the path is.
// For example, if we are trying to locate comments related to a field named
// `Address` in a message named `Person`, the path will be:
//
//	 [4, a, 2, b]
//
// While `a` gets determined by the order in which the messages appear in
// the proto file, and `b` is the field index specified in the proto
// file itself, the path actually needs to specify that `a` refers to a
// message and not, say, a service; and  that `b` refers to a field and not
// an option.
//
// protoPathIndex figures out the values 4 and 2 in the above example. Because
// messages are top level objects, the value of 4 comes from field id for
// `MessageType` inside `google.protobuf.descriptor.FileDescriptor` message.
// This field has a message type `google.protobuf.descriptor.DescriptorProto`.
// And inside message `DescriptorProto`, there is a field named `Field` with id
// 2.
//
// Some code generators seem to be hardcoding these values; this method instead
// interprets them from `descriptor.proto`-derived Go source as necessary.
func protoPathIndex(descriptorType reflect.Type, what string) int32 {
	field, ok := descriptorType.Elem().FieldByName(what)
	if !ok {
		panic(fmt.Errorf("could not find protobuf descriptor type id for %s", what))
	}
	pbtag := field.Tag.Get("protobuf")
	if pbtag == "" {
		panic(fmt.Errorf("no Go tag 'protobuf' on protobuf descriptor for %s", what))
	}
	path, err := strconv.Atoi(strings.Split(pbtag, ",")[1])
	if err != nil {
		panic(fmt.Errorf("protobuf descriptor id for %s cannot be converted to a number: %s", what, err.Error()))
	}

	return int32(path)
}

func extractFieldBehaviorFromFieldDescriptor(fd *descriptorpb.FieldDescriptorProto) ([]annotations.FieldBehavior, error) {
	if fd.Options == nil {
		return nil, nil
	}
	if !proto.HasExtension(fd.Options, annotations.E_FieldBehavior) {
		return nil, nil
	}
	ext := proto.GetExtension(fd.Options, annotations.E_FieldBehavior)
	opts, ok := ext.([]annotations.FieldBehavior)
	if !ok {
		return nil, fmt.Errorf("extension is %T; want a []FieldBehavior object", ext)
	}
	return opts, nil
}

func getFieldVisibilityOption(fd *descriptor.Field) *visibility.VisibilityRule {
	if fd.Options == nil {
		return nil
	}
	if !proto.HasExtension(fd.Options, visibility.E_FieldVisibility) {
		return nil
	}
	ext := proto.GetExtension(fd.Options, visibility.E_FieldVisibility)
	opts, ok := ext.(*visibility.VisibilityRule)
	if !ok {
		return nil
	}
	return opts
}

func getServiceVisibilityOption(fd *descriptor.Service) *visibility.VisibilityRule {
	if fd.Options == nil {
		return nil
	}
	if !proto.HasExtension(fd.Options, visibility.E_ApiVisibility) {
		return nil
	}
	ext := proto.GetExtension(fd.Options, visibility.E_ApiVisibility)
	opts, ok := ext.(*visibility.VisibilityRule)
	if !ok {
		return nil
	}
	return opts
}

func getMethodVisibilityOption(fd *descriptor.Method) *visibility.VisibilityRule {
	if fd.Options == nil {
		return nil
	}
	if !proto.HasExtension(fd.Options, visibility.E_MethodVisibility) {
		return nil
	}
	ext := proto.GetExtension(fd.Options, visibility.E_MethodVisibility)
	opts, ok := ext.(*visibility.VisibilityRule)
	if !ok {
		return nil
	}
	return opts
}

func getEnumValueVisibilityOption(fd *descriptorpb.EnumValueDescriptorProto) *visibility.VisibilityRule {
	if fd.Options == nil {
		return nil
	}
	if !proto.HasExtension(fd.Options, visibility.E_ValueVisibility) {
		return nil
	}
	ext := proto.GetExtension(fd.Options, visibility.E_ValueVisibility)
	opts, ok := ext.(*visibility.VisibilityRule)
	if !ok {
		return nil
	}
	return opts
}

func getFieldBehaviorOption(reg *descriptor.Registry, fd *descriptor.Field) ([]annotations.FieldBehavior, error) {
	opts, err := extractFieldBehaviorFromFieldDescriptor(fd.FieldDescriptorProto)
	if err != nil {
		return nil, err
	}
	if opts != nil {
		return opts, nil
	}
	return opts, nil
}

func openapiExamplesFromProtoExamples(in map[string]string) map[string]interface{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]interface{})
	for mimeType, exampleStr := range in {
		switch mimeType {
		case "application/json":
			// JSON example objects are rendered raw.
			out[mimeType] = json.RawMessage(exampleStr)
		default:
			// All other mimetype examples are rendered as strings.
			out[mimeType] = exampleStr
		}
	}
	return out
}

func protoJSONSchemaTypeToFormat(in []openapi_options.JSONSchema_JSONSchemaSimpleTypes) (string, string) {
	if len(in) == 0 {
		return "", ""
	}

	// Can't support more than 1 type, just return the first element.
	// This is due to an inconsistency in the design of the openapiv2 proto
	// and that used in schemaCore. schemaCore uses the v3 definition of types,
	// which only allows a single string, while the openapiv2 proto uses the OpenAPI v2
	// definition, which defers to the JSON schema definition, which allows a string or an array.
	// Sources:
	// https://swagger.io/specification/#itemsObject
	// https://tools.ietf.org/html/draft-fge-json-schema-validation-00#section-5.5.2
	switch in[0] {
	case openapi_options.JSONSchema_UNKNOWN, openapi_options.JSONSchema_NULL:
		return "", ""
	case openapi_options.JSONSchema_OBJECT:
		return "object", ""
	case openapi_options.JSONSchema_ARRAY:
		return "array", ""
	case openapi_options.JSONSchema_BOOLEAN:
		// NOTE: in OpenAPI specification, format should be empty on boolean type
		return "boolean", ""
	case openapi_options.JSONSchema_INTEGER:
		return "integer", "int32"
	case openapi_options.JSONSchema_NUMBER:
		return "number", "double"
	case openapi_options.JSONSchema_STRING:
		// NOTE: in OpenAPI specification, format should be empty on string type
		return "string", ""
	default:
		// Maybe panic?
		return "", ""
	}
}

func addCustomRefs(d openapi3.Schemas, reg *descriptor.Registry, refs refMap) error {
	if len(refs) == 0 {
		return nil
	}
	msgMap := make(messageMap)
	enumMap := make(enumMap)
	for ref := range refs {
		swgName, swgOk := fullyQualifiedNameToOpenAPIName(ref, reg)
		if !swgOk {
			glog.Errorf("can't resolve OpenAPI name from CustomRef '%v'", ref)
			continue
		}
		if _, ok := d[swgName]; ok {
			// Skip already existing definitions
			delete(refs, ref)
			continue
		}
		msg, err := reg.LookupMsg("", ref)
		if err == nil {
			msgMap[swgName] = msg
			continue
		}
		enum, err := reg.LookupEnum("", ref)
		if err == nil {
			enumMap[swgName] = enum
			continue
		}

		// ?? Should be either enum or msg
	}
	if err := renderMessagesAsDefinition(msgMap, d, reg, refs, nil); err != nil {
		return err
	}
	renderEnumerationsAsDefinition(enumMap, d, reg)

	// Run again in case any new refs were added
	return addCustomRefs(d, reg, refs)
}

func lowerCamelCase(fieldName string, fields []*descriptor.Field, msgs []*descriptor.Message) string {
	for _, oneField := range fields {
		if oneField.GetName() == fieldName {
			return oneField.GetJsonName()
		}
	}
	messageNameToFieldsToJSONName := make(map[string]map[string]string)
	fieldNameToType := make(map[string]string)
	for _, msg := range msgs {
		fieldNameToJSONName := make(map[string]string)
		for _, oneField := range msg.GetField() {
			fieldNameToJSONName[oneField.GetName()] = oneField.GetJsonName()
			fieldNameToType[oneField.GetName()] = oneField.GetTypeName()
		}
		messageNameToFieldsToJSONName[msg.GetName()] = fieldNameToJSONName
	}
	if strings.Contains(fieldName, ".") {
		fieldNames := strings.Split(fieldName, ".")
		fieldNamesWithCamelCase := make([]string, 0)
		for i := 0; i < len(fieldNames)-1; i++ {
			fieldNamesWithCamelCase = append(fieldNamesWithCamelCase, casing.JSONCamelCase(string(fieldNames[i])))
		}
		prefix := strings.Join(fieldNamesWithCamelCase, ".")
		reservedJSONName := getReservedJSONName(fieldName, messageNameToFieldsToJSONName, fieldNameToType)
		if reservedJSONName != "" {
			return prefix + "." + reservedJSONName
		}
	}
	return casing.JSONCamelCase(fieldName)
}

func getReservedJSONName(fieldName string, messageNameToFieldsToJSONName map[string]map[string]string, fieldNameToType map[string]string) string {
	if len(strings.Split(fieldName, ".")) == 2 {
		fieldNames := strings.Split(fieldName, ".")
		firstVariable := fieldNames[0]
		firstType := fieldNameToType[firstVariable]
		firstTypeShortNames := strings.Split(firstType, ".")
		firstTypeShortName := firstTypeShortNames[len(firstTypeShortNames)-1]
		return messageNameToFieldsToJSONName[firstTypeShortName][fieldNames[1]]
	}
	fieldNames := strings.Split(fieldName, ".")
	return getReservedJSONName(strings.Join(fieldNames[1:], "."), messageNameToFieldsToJSONName, fieldNameToType)
}

func find(a []string, x string) int {
	// This is a linear search but we are dealing with a small number of fields
	for i, n := range a {
		if x == n {
			return i
		}
	}
	return -1
}

// Make a deep copy of the outer parameters that has paramName as the first component,
// but remove the first component of the field path.
func subPathParams(paramName string, outerParams []descriptor.Parameter) []descriptor.Parameter {
	var innerParams []descriptor.Parameter
	for _, p := range outerParams {
		if len(p.FieldPath) > 1 && p.FieldPath[0].Name == paramName {
			subParam := descriptor.Parameter{
				FieldPath: p.FieldPath[1:],
				Target:    p.Target,
				Method:    p.Method,
			}
			innerParams = append(innerParams, subParam)
		}
	}
	return innerParams
}

func refXPath(name string) string {
	return "#/components/schemas/" + name
}
