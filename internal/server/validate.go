package server

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Error codes reported by the configuration validator.
const (
	codeInvalidJSON             = "invalid_json"
	codeInvalidDocument         = "invalid_document"
	codeDuplicateIdentifier     = "duplicate_identifier"
	codeUnknownResourceType     = "unknown_resource_type"
	codeTypeMismatch            = "type_mismatch"
	codeMissingRequiredProperty = "missing_required_property"
	codeUnexpectedProperty      = "unexpected_property"
	codeUnknownDependency       = "unknown_dependency"
	codeSelfDependency          = "self_dependency"
	codeDuplicateDependency     = "duplicate_dependency"
	codeDependencyCycle         = "dependency_cycle"
)

// validationError is one machine-readable finding. Path is an RFC 6901 JSON
// Pointer into the submitted document.
type validationError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Path    string `json:"path"`
}

type validateResponse struct {
	Valid  bool              `json:"valid"`
	Errors []validationError `json:"errors"`
}

// handleValidateConfiguration validates a configuration document without
// creating or persisting anything.
func handleValidateConfiguration(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"code": "method_not_allowed"},
		})
		return
	}

	doc, ok := decodeConfigurationBody(w, r)
	if !ok {
		return
	}

	errs := validateConfiguration(doc)
	sortValidationErrors(errs)
	if len(errs) == 0 {
		writeValidation(w, http.StatusOK, nil)
		return
	}
	writeValidation(w, http.StatusUnprocessableEntity, errs)
}

// decodeConfigurationBody reads the single JSON value expected from a
// configuration request body. It writes the shared 400 invalid_json response
// and returns ok=false when the body is empty, malformed, or trailing data.
func decodeConfigurationBody(w http.ResponseWriter, r *http.Request) (any, bool) {
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		writeValidation(w, http.StatusBadRequest, []validationError{{
			Code:    codeInvalidJSON,
			Message: "request body is empty or is not valid JSON",
			Path:    "",
		}})
		return nil, false
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		writeValidation(w, http.StatusBadRequest, []validationError{{
			Code:    codeInvalidJSON,
			Message: "request body must contain exactly one JSON value",
			Path:    "",
		}})
		return nil, false
	}
	return doc, true
}

// sortValidationErrors orders findings by JSON Pointer path, then by code.
func sortValidationErrors(errs []validationError) {
	sort.SliceStable(errs, func(i, j int) bool {
		if errs[i].Path != errs[j].Path {
			return errs[i].Path < errs[j].Path
		}
		return errs[i].Code < errs[j].Code
	})
}

func writeValidation(w http.ResponseWriter, status int, errs []validationError) {
	if errs == nil {
		errs = []validationError{}
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(validateResponse{Valid: len(errs) == 0, Errors: errs})
}

// pointerEscape escapes a single JSON Pointer path segment per RFC 6901.
func pointerEscape(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	return strings.ReplaceAll(s, "/", "~1")
}

// validateConfiguration checks a decoded document and returns every
// independently determinable problem, in discovery order.
func validateConfiguration(doc any) []validationError {
	_, errs := resolveConfiguration(doc)
	return errs
}

// resolveConfiguration validates a decoded configuration document exactly
// like validateConfiguration and additionally returns the resolved view of
// the document: every variable reference inside resource properties is
// replaced by the referenced variable's literal value. A document naming
// neither variables nor variableValues is returned unchanged, so historical
// configurations keep their exact behavior. The resolved document is
// meaningful only when the returned error list is empty.
func resolveConfiguration(doc any) (any, []validationError) {
	var errs []validationError
	add := func(code, message, path string) {
		errs = append(errs, validationError{Code: code, Message: message, Path: path})
	}

	root, ok := doc.(map[string]any)
	if !ok {
		add(codeInvalidDocument, "document must be a JSON object", "")
		return nil, errs
	}
	for key := range root {
		if key != "resourceTypes" && key != "resources" && key != "variables" && key != "variableValues" {
			add(codeInvalidDocument, fmt.Sprintf("unknown field %q", key), "/"+pointerEscape(key))
		}
	}

	typesRaw, hasTypes := root["resourceTypes"]
	if !hasTypes {
		add(codeInvalidDocument, `missing required field "resourceTypes"`, "")
	}
	resourcesRaw, hasResources := root["resources"]
	if !hasResources {
		add(codeInvalidDocument, `missing required field "resources"`, "")
	}

	var typeEntries []any
	if hasTypes {
		arr, ok := typesRaw.([]any)
		if !ok {
			add(codeInvalidDocument, `"resourceTypes" must be an array`, "/resourceTypes")
		} else {
			typeEntries = arr
		}
	}
	var resourceEntries []any
	if hasResources {
		arr, ok := resourcesRaw.([]any)
		if !ok {
			add(codeInvalidDocument, `"resources" must be an array`, "/resources")
		} else {
			resourceEntries = arr
		}
	}

	// declared holds every non-empty type name seen, even when its entry is
	// invalid or a duplicate; usable holds only names with a valid, unique
	// schema. Resources referencing a declared-but-unusable name are spared
	// property-level errors.
	declared := map[string]bool{}
	usable := map[string]any{}
	for i, entry := range typeEntries {
		entryPath := fmt.Sprintf("/resourceTypes/%d", i)
		obj, ok := entry.(map[string]any)
		if !ok {
			add(codeInvalidDocument, "resource type entry must be an object", entryPath)
			continue
		}
		name, nameValid := identifierField(obj, "name", entryPath, add)
		schemaRaw, hasSchema := obj["schema"]
		schemaValid := false
		if !hasSchema {
			add(codeInvalidDocument, `missing required field "schema"`, entryPath)
		} else {
			before := len(errs)
			validateSchema(schemaRaw, entryPath+"/schema", add)
			schemaValid = len(errs) == before
		}
		if !nameValid {
			continue
		}
		if declared[name] {
			add(codeDuplicateIdentifier, fmt.Sprintf("duplicate resource type name %q", name), entryPath+"/name")
			delete(usable, name)
			continue
		}
		declared[name] = true
		if schemaValid {
			usable[name] = schemaRaw
		}
	}

	// The variable phase runs before property validation, dependency
	// ordering, and diff computation: references inside resource properties
	// are resolved against the declared variables and explicit values, and
	// any problem it finds suppresses property validation, because
	// unresolved properties cannot be checked against their schemas.
	variableErrs := len(errs)
	decls, variablesActive := checkVariableDeclarations(root, add)
	resolved := doc
	var resolvedEntries []any
	if variablesActive {
		resolvedEntries = make([]any, len(resourceEntries))
		for i, entry := range resourceEntries {
			obj, ok := entry.(map[string]any)
			if !ok {
				resolvedEntries[i] = entry
				continue
			}
			props, ok := obj["properties"].(map[string]any)
			if !ok {
				resolvedEntries[i] = entry
				continue
			}
			resolvedObj := make(map[string]any, len(obj))
			for key, value := range obj {
				resolvedObj[key] = value
			}
			resolvedObj["properties"] = resolveVariableReferences(props, fmt.Sprintf("/resources/%d/properties", i), decls, add)
			resolvedEntries[i] = resolvedObj
		}
		resolvedRoot := make(map[string]any, len(root))
		for key, value := range root {
			resolvedRoot[key] = value
		}
		resolvedRoot["resources"] = resolvedEntries
		resolved = resolvedRoot
	}
	variablesOK := len(errs) == variableErrs

	seenAddresses := map[string]bool{}
	for i, entry := range resourceEntries {
		entryPath := fmt.Sprintf("/resources/%d", i)
		obj, ok := entry.(map[string]any)
		if !ok {
			add(codeInvalidDocument, "resource entry must be an object", entryPath)
			continue
		}
		if address, ok := identifierField(obj, "address", entryPath, add); ok {
			if seenAddresses[address] {
				add(codeDuplicateIdentifier, fmt.Sprintf("duplicate resource address %q", address), entryPath+"/address")
			} else {
				seenAddresses[address] = true
			}
		}

		var schema any
		schemaUsable := false
		typeRaw, hasType := obj["type"]
		typeName, typeIsString := typeRaw.(string)
		switch {
		case !hasType:
			add(codeInvalidDocument, `missing required field "type"`, entryPath)
		case !typeIsString:
			add(codeInvalidDocument, `"type" must be a string`, entryPath+"/type")
		case typeName == "":
			add(codeInvalidDocument, `"type" must not be empty`, entryPath+"/type")
		case !declared[typeName]:
			add(codeUnknownResourceType, fmt.Sprintf("unknown resource type %q", typeName), entryPath+"/type")
		default:
			if s, ok := usable[typeName]; ok {
				schema, schemaUsable = s, true
			}
		}

		propsRaw, hasProps := obj["properties"]
		if !hasProps {
			add(codeInvalidDocument, `missing required field "properties"`, entryPath)
			continue
		}
		props, ok := propsRaw.(map[string]any)
		if !ok {
			add(codeInvalidDocument, `"properties" must be an object`, entryPath+"/properties")
			continue
		}
		if schemaUsable && variablesOK {
			// Properties are validated against their schema only after
			// variable resolution has replaced every reference with its
			// literal value.
			validateValue(resolvedPropertiesOf(resolvedEntries, i, props), schema, entryPath+"/properties", add)
		}
	}

	return resolved, errs
}

// resolvedPropertiesOf returns the variable-resolved properties of resource
// entry i, falling back to the original properties when the variable phase
// was inactive for the document.
func resolvedPropertiesOf(resolvedEntries []any, i int, props map[string]any) map[string]any {
	if resolvedEntries == nil {
		return props
	}
	if obj, ok := resolvedEntries[i].(map[string]any); ok {
		if resolved, ok := obj["properties"].(map[string]any); ok {
			return resolved
		}
	}
	return props
}

// identifierField extracts a required non-empty string field, reporting
// invalid_document for a missing, non-string, or empty value.
func identifierField(obj map[string]any, field, path string, add func(code, message, path string)) (string, bool) {
	raw, ok := obj[field]
	if !ok {
		add(codeInvalidDocument, fmt.Sprintf("missing required field %q", field), path)
		return "", false
	}
	s, ok := raw.(string)
	if !ok {
		add(codeInvalidDocument, fmt.Sprintf("%q must be a string", field), path+"/"+field)
		return "", false
	}
	if s == "" {
		add(codeInvalidDocument, fmt.Sprintf("%q must not be empty", field), path+"/"+field)
		return "", false
	}
	return s, true
}

// validateSchema checks the structural legality of a schema, recursing into
// array items and object properties.
func validateSchema(schema any, path string, add func(code, message, path string)) {
	obj, ok := schema.(map[string]any)
	if !ok {
		add(codeInvalidDocument, "schema must be an object", path)
		return
	}
	typeRaw, hasType := obj["type"]
	typeName, isString := typeRaw.(string)
	if !hasType {
		add(codeInvalidDocument, `missing required field "type"`, path)
		return
	}
	if !isString {
		add(codeInvalidDocument, `"type" must be a string`, path+"/type")
		return
	}
	switch typeName {
	case "string", "integer", "number", "boolean":
	case "array":
		items, has := obj["items"]
		if !has {
			add(codeInvalidDocument, `array schema requires "items"`, path)
			return
		}
		validateSchema(items, path+"/items", add)
	case "object":
		if propsRaw, has := obj["properties"]; has {
			props, ok := propsRaw.(map[string]any)
			if !ok {
				add(codeInvalidDocument, `"properties" must be an object`, path+"/properties")
			} else {
				names := make([]string, 0, len(props))
				for name := range props {
					names = append(names, name)
				}
				sort.Strings(names)
				for _, name := range names {
					validateSchema(props[name], path+"/properties/"+pointerEscape(name), add)
				}
			}
		}
		if reqRaw, has := obj["required"]; has {
			req, ok := reqRaw.([]any)
			if !ok {
				add(codeInvalidDocument, `"required" must be an array of strings`, path+"/required")
			} else {
				for j, entry := range req {
					if _, ok := entry.(string); !ok {
						add(codeInvalidDocument, `"required" entries must be strings`, fmt.Sprintf("%s/required/%d", path, j))
					}
				}
			}
		}
		if apRaw, has := obj["additionalProperties"]; has {
			if _, ok := apRaw.(bool); !ok {
				add(codeInvalidDocument, `"additionalProperties" must be a boolean`, path+"/additionalProperties")
			}
		}
	default:
		add(codeInvalidDocument, fmt.Sprintf("unknown schema type %q", typeName), path+"/type")
	}
}

// validateValue checks a value against an already-validated schema.
func validateValue(value, schema any, path string, add func(code, message, path string)) {
	obj, ok := schema.(map[string]any)
	if !ok {
		return
	}
	typeName, _ := obj["type"].(string)
	switch typeName {
	case "string":
		if _, ok := value.(string); !ok {
			add(codeTypeMismatch, fmt.Sprintf("expected string, got %s", jsonTypeName(value)), path)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			add(codeTypeMismatch, fmt.Sprintf("expected boolean, got %s", jsonTypeName(value)), path)
		}
	case "integer":
		n, ok := value.(json.Number)
		if !ok || !isJSONInteger(n) {
			add(codeTypeMismatch, fmt.Sprintf("expected integer, got %s", jsonTypeName(value)), path)
		}
	case "number":
		if _, ok := value.(json.Number); !ok {
			add(codeTypeMismatch, fmt.Sprintf("expected number, got %s", jsonTypeName(value)), path)
		}
	case "array":
		arr, ok := value.([]any)
		if !ok {
			add(codeTypeMismatch, fmt.Sprintf("expected array, got %s", jsonTypeName(value)), path)
			return
		}
		items := obj["items"]
		for i, el := range arr {
			validateValue(el, items, fmt.Sprintf("%s/%d", path, i), add)
		}
	case "object":
		rec, ok := value.(map[string]any)
		if !ok {
			add(codeTypeMismatch, fmt.Sprintf("expected object, got %s", jsonTypeName(value)), path)
			return
		}
		props, _ := obj["properties"].(map[string]any)
		additional := false
		if ap, ok := obj["additionalProperties"].(bool); ok {
			additional = ap
		}
		seen := map[string]bool{}
		if reqRaw, ok := obj["required"].([]any); ok {
			for _, entry := range reqRaw {
				name, ok := entry.(string)
				if !ok || seen[name] {
					continue
				}
				seen[name] = true
				if _, present := rec[name]; !present {
					add(codeMissingRequiredProperty, fmt.Sprintf("missing required property %q", name), path)
				}
			}
		}
		names := make([]string, 0, len(rec))
		for name := range rec {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			childPath := path + "/" + pointerEscape(name)
			if sub, declared := props[name]; declared {
				validateValue(rec[name], sub, childPath, add)
			} else if !additional {
				add(codeUnexpectedProperty, fmt.Sprintf("unexpected property %q", name), childPath)
			}
		}
	}
}

// isJSONInteger reports whether a JSON number literal denotes an integer.
// Whole-valued forms such as 1.0 or 1e2 count as integers; fractions do not.
func isJSONInteger(n json.Number) bool {
	s := n.String()
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return false
	}
	return !math.IsInf(f, 0) && f == math.Trunc(f)
}

func jsonTypeName(v any) string {
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
		return "value"
	}
}
