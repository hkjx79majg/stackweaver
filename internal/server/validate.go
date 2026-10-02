package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Error codes reported by the configuration validation endpoint.
const (
	codeInvalidJSON         = "invalid_json"
	codeInvalidDocument     = "invalid_document"
	codeDuplicateIdentifier = "duplicate_identifier"
	codeUnknownResourceType = "unknown_resource_type"
	codeTypeMismatch        = "type_mismatch"
	codeMissingRequired     = "missing_required_property"
	codeUnexpectedProperty  = "unexpected_property"
)

// validationError is a single independently-determinable problem. Path is an
// RFC 6901 JSON Pointer into the request document.
type validationError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Path    string `json:"path"`
}

// validationResponse is the result envelope returned by the endpoint.
type validationResponse struct {
	Valid  bool              `json:"valid"`
	Errors []validationError `json:"errors"`
}

// validateConfiguration parses and validates a configuration document. The
// boolean is false when the body is not exactly one JSON document (empty,
// malformed, or followed by a second value); the caller then answers 400.
func validateConfiguration(body []byte) ([]validationError, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, false
	}
	// The body must contain exactly one JSON value: any trailing token,
	// including a second JSON value, makes it invalid JSON for our purposes.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, false
	}
	return validateDocument(doc), true
}

// escapeToken escapes one RFC 6901 reference token.
func escapeToken(token string) string {
	token = strings.ReplaceAll(token, "~", "~0")
	return strings.ReplaceAll(token, "/", "~1")
}

// pointer joins a base pointer with an object property token.
func pointer(base, token string) string {
	return base + "/" + escapeToken(token)
}

// indexPath joins a base pointer with an array index.
func indexPath(base string, i int) string {
	return base + "/" + strconv.Itoa(i)
}

func invalidDoc(path, message string) validationError {
	return validationError{Code: codeInvalidDocument, Message: message, Path: path}
}

// validateDocument runs every independently-determinable check against a
// parsed document and returns errors sorted by path, then code.
func validateDocument(doc any) []validationError {
	var errs []validationError

	root, ok := doc.(map[string]any)
	if !ok {
		return []validationError{invalidDoc("", "document must be a JSON object")}
	}

	for _, key := range []string{"resourceTypes", "resources"} {
		if _, ok := root[key]; !ok {
			errs = append(errs, invalidDoc("", fmt.Sprintf("root object must contain %q", key)))
		}
	}
	for key := range root {
		if key != "resourceTypes" && key != "resources" {
			errs = append(errs, validationError{
				Code:    codeUnexpectedProperty,
				Message: fmt.Sprintf("root object does not accept property %q", key),
				Path:    pointer("", key),
			})
		}
	}

	// type name -> declaring index; duplicates and structurally invalid types
	// are tracked so referencing resources get unknown_resource_type only.
	typeNames := make(map[string]int)
	typeDuplicate := make(map[string]bool)
	typeUsable := make(map[int]bool)
	var typeSchemas []any

	rtRaw, rtPresent := root["resourceTypes"]
	if rtPresent {
		types, ok := rtRaw.([]any)
		if !ok {
			errs = append(errs, invalidDoc("/resourceTypes", `"resourceTypes" must be an array`))
		} else {
			typeSchemas = make([]any, len(types))
			for i, item := range types {
				itemPath := indexPath("/resourceTypes", i)

				obj, ok := item.(map[string]any)
				if !ok {
					errs = append(errs, invalidDoc(itemPath, "resource type entry must be an object"))
					continue
				}

				nameUnique := false
				if name, present := obj["name"]; !present {
					errs = append(errs, invalidDoc(itemPath, `resource type entry must contain "name"`))
				} else if ns, isStr := name.(string); !isStr {
					errs = append(errs, invalidDoc(itemPath+"/name", `"name" must be a string`))
				} else if ns == "" {
					errs = append(errs, invalidDoc(itemPath+"/name", `"name" must not be empty`))
				} else if _, seen := typeNames[ns]; seen {
					typeDuplicate[ns] = true
					errs = append(errs, validationError{
						Code:    codeDuplicateIdentifier,
						Message: fmt.Sprintf("duplicate resource type name %q", ns),
						Path:    itemPath + "/name",
					})
				} else {
					typeNames[ns] = i
					nameUnique = true
				}

				schemaOK := false
				if schema, present := obj["schema"]; !present {
					errs = append(errs, invalidDoc(itemPath, `resource type entry must contain "schema"`))
				} else {
					schemaErrs := validateSchemaShape(schema, itemPath+"/schema")
					errs = append(errs, schemaErrs...)
					schemaOK = len(schemaErrs) == 0
					typeSchemas[i] = schema
				}

				for key := range obj {
					if key != "name" && key != "schema" {
						errs = append(errs, validationError{
							Code:    codeUnexpectedProperty,
							Message: fmt.Sprintf("resource type entry does not accept property %q", key),
							Path:    pointer(itemPath, key),
						})
					}
				}

				typeUsable[i] = nameUnique && schemaOK
			}
		}
	}

	// Resolve a declared type name to its schema only when unique and legal.
	schemas := make(map[string]any)
	for name, idx := range typeNames {
		if !typeDuplicate[name] && typeUsable[idx] {
			schemas[name] = typeSchemas[idx]
		}
	}

	if resRaw, present := root["resources"]; present {
		resources, ok := resRaw.([]any)
		if !ok {
			errs = append(errs, invalidDoc("/resources", `"resources" must be an array`))
		} else {
			seenAddresses := make(map[string]bool)
			for i, item := range resources {
				itemPath := indexPath("/resources", i)

				obj, ok := item.(map[string]any)
				if !ok {
					errs = append(errs, invalidDoc(itemPath, "resource entry must be an object"))
					continue
				}

				if address, present := obj["address"]; !present {
					errs = append(errs, invalidDoc(itemPath, `resource entry must contain "address"`))
				} else if as, isStr := address.(string); !isStr {
					errs = append(errs, invalidDoc(itemPath+"/address", `"address" must be a string`))
				} else if as == "" {
					errs = append(errs, invalidDoc(itemPath+"/address", `"address" must not be empty`))
				} else if seenAddresses[as] {
					errs = append(errs, validationError{
						Code:    codeDuplicateIdentifier,
						Message: fmt.Sprintf("duplicate resource address %q", as),
						Path:    itemPath + "/address",
					})
				} else {
					seenAddresses[as] = true
				}

				typeName := ""
				typeNameOK := false
				if tn, present := obj["type"]; !present {
					errs = append(errs, invalidDoc(itemPath, `resource entry must contain "type"`))
				} else if tns, isStr := tn.(string); !isStr {
					errs = append(errs, invalidDoc(itemPath+"/type", `"type" must be a string`))
				} else {
					typeName = tns
					typeNameOK = true
					if _, known := schemas[tns]; !known {
						errs = append(errs, validationError{
							Code:    codeUnknownResourceType,
							Message: fmt.Sprintf("unknown resource type %q", tns),
							Path:    itemPath + "/type",
						})
					}
				}

				props, propsPresent := obj["properties"]
				if !propsPresent {
					errs = append(errs, invalidDoc(itemPath, `resource entry must contain "properties"`))
				} else if _, isObj := props.(map[string]any); !isObj {
					errs = append(errs, invalidDoc(itemPath+"/properties", `"properties" must be an object`))
				} else if typeNameOK {
					// Property-level checks run only for a declared, usable type;
					// illegal or duplicate types stop here.
					if schema, known := schemas[typeName]; known {
						errs = append(errs, validateValue(props, schema, itemPath+"/properties")...)
					}
				}

				for key := range obj {
					if key != "address" && key != "type" && key != "properties" {
						errs = append(errs, validationError{
							Code:    codeUnexpectedProperty,
							Message: fmt.Sprintf("resource entry does not accept property %q", key),
							Path:    pointer(itemPath, key),
						})
					}
				}
			}
		}
	}

	sortErrors(errs)
	return errs
}

// validateSchemaShape checks a schema's structure, returning one
// invalid_document (or unexpected_property) error per problem. Only an empty
// result is usable for data validation.
func validateSchemaShape(schema any, path string) []validationError {
	obj, ok := schema.(map[string]any)
	if !ok {
		return []validationError{invalidDoc(path, "schema must be an object")}
	}

	tv, present := obj["type"]
	if !present {
		return []validationError{invalidDoc(path, `schema must contain "type"`)}
	}
	t, isStr := tv.(string)
	if !isStr {
		return []validationError{invalidDoc(path+"/type", `"type" must be a string`)}
	}

	var errs []validationError
	switch t {
	case "string", "integer", "number", "boolean":
	case "array":
		items, present := obj["items"]
		if !present {
			errs = append(errs, invalidDoc(path, `array schema must declare "items"`))
		} else {
			errs = append(errs, validateSchemaShape(items, path+"/items")...)
		}
	case "object":
		if props, present := obj["properties"]; present {
			pm, isObj := props.(map[string]any)
			if !isObj {
				errs = append(errs, invalidDoc(path+"/properties", `"properties" must be an object`))
			} else {
				keys := make([]string, 0, len(pm))
				for k := range pm {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					errs = append(errs, validateSchemaShape(pm[k], path+"/properties/"+escapeToken(k))...)
				}
			}
		}
		if req, present := obj["required"]; present {
			ra, isArr := req.([]any)
			if !isArr {
				errs = append(errs, invalidDoc(path+"/required", `"required" must be an array`))
			} else {
				for i, r := range ra {
					if _, isStr := r.(string); !isStr {
						errs = append(errs, invalidDoc(indexPath(path+"/required", i), "required entries must be strings"))
					}
				}
			}
		}
		if ap, present := obj["additionalProperties"]; present {
			if _, isBool := ap.(bool); !isBool {
				errs = append(errs, invalidDoc(path+"/additionalProperties", `"additionalProperties" must be a boolean`))
			}
		}
	default:
		errs = append(errs, invalidDoc(path+"/type", fmt.Sprintf("unknown schema type %q", t)))
	}

	for key := range obj {
		switch key {
		case "type", "items", "properties", "required", "additionalProperties":
		default:
			errs = append(errs, invalidDoc(pointer(path, key), fmt.Sprintf("schema does not accept keyword %q", key)))
		}
	}
	return errs
}

// validateValue checks data against a structurally-valid schema. It only
// reports type_mismatch, missing_required_property and unexpected_property.
func validateValue(value any, schema any, path string) []validationError {
	obj := schema.(map[string]any)
	t := obj["type"].(string)

	switch t {
	case "string":
		if _, ok := value.(string); !ok {
			return mismatch(path, "string", value)
		}
	case "integer":
		n, ok := value.(json.Number)
		if !ok || !isIntegerToken(n.String()) {
			// No implicit conversion and no decimals/exponents: an integer is
			// a JSON number token without a fraction or exponent part.
			return mismatch(path, "integer", value)
		}
	case "number":
		if _, ok := value.(json.Number); !ok {
			// Integers and decimals are both numbers; the decoder already
			// guaranteed the token is a valid JSON number.
			return mismatch(path, "number", value)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return mismatch(path, "boolean", value)
		}
	case "array":
		arr, ok := value.([]any)
		if !ok {
			return mismatch(path, "array", value)
		}
		var errs []validationError
		for i, v := range arr {
			errs = append(errs, validateValue(v, obj["items"], indexPath(path, i))...)
		}
		return errs
	case "object":
		m, ok := value.(map[string]any)
		if !ok {
			return mismatch(path, "object", value)
		}
		var errs []validationError

		if req, ok := obj["required"].([]any); ok {
			seen := make(map[string]bool)
			for _, r := range req {
				name := r.(string)
				if seen[name] {
					continue
				}
				seen[name] = true
				if _, present := m[name]; !present {
					errs = append(errs, validationError{
						Code:    codeMissingRequired,
						Message: fmt.Sprintf("missing required property %q", name),
						Path:    pointer(path, name),
					})
				}
			}
		}

		additional := false
		if ap, ok := obj["additionalProperties"].(bool); ok {
			additional = ap
		}

		props, _ := obj["properties"].(map[string]any)
		for k, v := range m {
			if sub, declared := props[k]; declared {
				errs = append(errs, validateValue(v, sub, pointer(path, k))...)
			} else if !additional {
				errs = append(errs, validationError{
					Code:    codeUnexpectedProperty,
					Message: fmt.Sprintf("unexpected property %q", k),
					Path:    pointer(path, k),
				})
			}
		}
		return errs
	}
	return nil
}

// isIntegerToken reports whether a valid JSON number token is written without
// a fraction or exponent (e.g. "12" or "-3"), which is what the integer type
// accepts. "1.0" and "1e2" are valid numbers but not integers.
func isIntegerToken(token string) bool {
	i := 0
	if len(token) > 0 && token[0] == '-' {
		i++
	}
	if i >= len(token) {
		return false
	}
	for ; i < len(token); i++ {
		if token[i] < '0' || token[i] > '9' {
			return false
		}
	}
	return true
}

func mismatch(path, expected string, value any) []validationError {
	return []validationError{{
		Code:    codeTypeMismatch,
		Message: fmt.Sprintf("expected %s, got %s", expected, jsonKind(value)),
		Path:    path,
	}}
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}

func sortErrors(errs []validationError) {
	sort.SliceStable(errs, func(i, j int) bool {
		if errs[i].Path != errs[j].Path {
			return errs[i].Path < errs[j].Path
		}
		return errs[i].Code < errs[j].Code
	})
}
