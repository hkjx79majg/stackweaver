package server

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Error codes reported by the variable phase of configuration validation,
// alongside the shared invalid_document and type_mismatch codes.
const (
	codeInvalidVariableReference = "invalid_variable_reference"
	codeUnknownVariable          = "unknown_variable"
	codeMissingVariableValue     = "missing_variable_value"
)

// variableDeclaration is the validated view of one entry of the top-level
// variables object, plus the explicit value variableValues assigns to it, if
// any. Explicit values take precedence over defaults; both are literal JSON
// values and are never scanned for further references.
type variableDeclaration struct {
	schema       any
	schemaUsable bool
	hasDefault   bool
	def          any
	hasValue     bool
	value        any
}

// sortedKeys returns the keys of m in ascending order, so the variable phase
// discovers problems deterministically.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// checkVariableDeclarations validates the optional top-level variables and
// variableValues fields of a configuration document: declarations must be
// objects pairing a non-empty name with a legal schema and an optional
// default, explicit values must name a declared variable, and every default
// and explicit value must conform to its variable's schema, whether or not
// any resource references it. It reports whether the variable phase is
// active at all — a document naming neither field keeps the historical
// behavior, in which a "$variable" key inside properties is ordinary data.
func checkVariableDeclarations(root map[string]any, add func(code, message, path string)) (map[string]variableDeclaration, bool) {
	_, hasVariables := root["variables"]
	_, hasValues := root["variableValues"]
	if !hasVariables && !hasValues {
		return nil, false
	}

	decls := map[string]variableDeclaration{}
	if raw, present := root["variables"]; present {
		obj, ok := raw.(map[string]any)
		if !ok {
			add(codeInvalidDocument, `"variables" must be an object`, "/variables")
		} else {
			for _, name := range sortedKeys(obj) {
				entryPath := "/variables/" + pointerEscape(name)
				if name == "" {
					add(codeInvalidDocument, "variable name must not be empty", entryPath)
					continue
				}
				declObj, ok := obj[name].(map[string]any)
				if !ok {
					add(codeInvalidDocument, "variable declaration must be an object", entryPath)
					decls[name] = variableDeclaration{}
					continue
				}
				for _, key := range sortedKeys(declObj) {
					if key != "schema" && key != "default" {
						add(codeInvalidDocument, fmt.Sprintf("unknown field %q", key), entryPath+"/"+pointerEscape(key))
					}
				}
				decl := variableDeclaration{}
				schemaRaw, hasSchema := declObj["schema"]
				if !hasSchema {
					add(codeInvalidDocument, `missing required field "schema"`, entryPath)
				} else {
					schemaValid := true
					validateSchema(schemaRaw, entryPath+"/schema", func(code, message, path string) {
						schemaValid = false
						add(code, message, path)
					})
					decl.schema = schemaRaw
					decl.schemaUsable = schemaValid
				}
				if def, has := declObj["default"]; has {
					decl.hasDefault = true
					decl.def = def
				}
				decls[name] = decl
			}
		}
	}

	if raw, present := root["variableValues"]; present {
		obj, ok := raw.(map[string]any)
		if !ok {
			add(codeInvalidDocument, `"variableValues" must be an object`, "/variableValues")
		} else {
			for _, name := range sortedKeys(obj) {
				decl, declared := decls[name]
				if !declared {
					add(codeUnknownVariable, fmt.Sprintf("unknown variable %q", name), "/variableValues/"+pointerEscape(name))
					continue
				}
				decl.hasValue = true
				decl.value = obj[name]
				decls[name] = decl
			}
		}
	}

	// Defaults and explicit values are checked against their variable's
	// schema even when nothing references them. Values are literal: a
	// reference-shaped object inside one is data, not a nested reference.
	for _, name := range sortedKeys(decls) {
		decl := decls[name]
		if !decl.schemaUsable {
			continue
		}
		if decl.hasDefault && !valueMatchesSchema(decl.def, decl.schema) {
			add(codeTypeMismatch, fmt.Sprintf("default of variable %q does not match its schema", name), "/variables/"+pointerEscape(name)+"/default")
		}
		if decl.hasValue && !valueMatchesSchema(decl.value, decl.schema) {
			add(codeTypeMismatch, fmt.Sprintf("value of variable %q does not match its schema", name), "/variableValues/"+pointerEscape(name))
		}
	}
	return decls, true
}

// resolveVariableReferences replaces every variable reference in value with
// the referenced variable's literal value. A reference is an object whose
// only field is "$variable" holding a non-empty variable name; an object
// mentioning "$variable" alongside other fields or with an illegal name is
// an invalid reference. Substituted values are literal and never scanned
// again. path is the JSON Pointer of value within the submitted document.
func resolveVariableReferences(value any, path string, decls map[string]variableDeclaration, add func(code, message, path string)) any {
	switch v := value.(type) {
	case map[string]any:
		if refRaw, references := v["$variable"]; references {
			if len(v) != 1 {
				add(codeInvalidVariableReference, `variable reference object must not have fields besides "$variable"`, path)
				return value
			}
			ref, ok := refRaw.(string)
			if !ok || ref == "" {
				add(codeInvalidVariableReference, `"$variable" must be a non-empty variable name`, path)
				return value
			}
			decl, declared := decls[ref]
			if !declared {
				add(codeUnknownVariable, fmt.Sprintf("unknown variable %q", ref), path)
				return value
			}
			if !decl.hasValue && !decl.hasDefault {
				add(codeMissingVariableValue, fmt.Sprintf("variable %q has neither an explicit value nor a default", ref), "/variables/"+pointerEscape(ref))
				return value
			}
			if decl.hasValue {
				return decl.value
			}
			return decl.def
		}
		resolved := make(map[string]any, len(v))
		for key, item := range v {
			resolved[key] = resolveVariableReferences(item, path+"/"+pointerEscape(key), decls, add)
		}
		return resolved
	case []any:
		resolved := make([]any, len(v))
		for i, item := range v {
			resolved[i] = resolveVariableReferences(item, fmt.Sprintf("%s/%d", path, i), decls, add)
		}
		return resolved
	default:
		return value
	}
}

// valueMatchesSchema reports whether value conforms to an already-validated
// schema, following the same rules validateValue enforces for resource
// properties. It backs the variable phase, where any nonconformance of a
// default or explicit value surfaces as a single type_mismatch finding at
// the value's own path.
func valueMatchesSchema(value, schema any) bool {
	obj, ok := schema.(map[string]any)
	if !ok {
		return true
	}
	typeName, _ := obj["type"].(string)
	switch typeName {
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "integer":
		n, ok := value.(json.Number)
		return ok && isJSONInteger(n)
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "array":
		arr, ok := value.([]any)
		if !ok {
			return false
		}
		for _, el := range arr {
			if !valueMatchesSchema(el, obj["items"]) {
				return false
			}
		}
		return true
	case "object":
		rec, ok := value.(map[string]any)
		if !ok {
			return false
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
					return false
				}
			}
		}
		for name, item := range rec {
			if sub, declared := props[name]; declared {
				if !valueMatchesSchema(item, sub) {
					return false
				}
			} else if !additional {
				return false
			}
		}
		return true
	default:
		return true
	}
}
