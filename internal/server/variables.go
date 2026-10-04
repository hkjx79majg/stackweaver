package server

import (
	"fmt"
	"sort"
)

// Error codes reported for the variable sections of a configuration document
// and for the "$variable" references inside resource properties.
const (
	codeInvalidVariableReference = "invalid_variable_reference"
	codeUnknownVariable          = "unknown_variable"
	codeMissingVariableValue     = "missing_variable_value"
)

// variableEnv is the resolved view of a document's variable sections:
// declared holds every declared name and values holds the effective value of
// every declaration that has one (explicit value winning over default).
// missing tracks the names already reported as missing_variable_value so a
// declaration referenced twice produces one finding.
type variableEnv struct {
	declared map[string]bool
	values   map[string]any
	missing  map[string]bool
}

// variablesActivated reports whether the document uses the variable feature:
// either root field is present. Documents with neither field keep the exact
// pre-variable behavior, including how "$variable"-shaped properties are
// treated.
func variablesActivated(root map[string]any) bool {
	if _, ok := root["variables"]; ok {
		return true
	}
	_, ok := root["variableValues"]
	return ok
}

// analyzeVariables validates the variables and variableValues sections of
// root and returns the effective environment together with every finding.
// Declarations and values are checked even when nothing references them;
// explicit values and defaults are checked against their declaration's
// schema with the same rules resource properties use. The returned
// environment is only meaningful when the error list is empty.
func analyzeVariables(root map[string]any) (*variableEnv, []validationError) {
	env := &variableEnv{
		declared: map[string]bool{},
		values:   map[string]any{},
		missing:  map[string]bool{},
	}
	var errs []validationError
	add := func(code, message, path string) {
		errs = append(errs, validationError{Code: code, Message: message, Path: path})
	}

	// schemas holds the valid schema of each syntactically legal declaration;
	// a declaration with an invalid schema still counts as declared, so values
	// naming it are spared a cascading unknown_variable.
	schemas := map[string]any{}
	defaults := map[string]any{}
	declaredBroken := map[string]bool{}
	if raw, present := root["variables"]; present {
		decls, ok := raw.(map[string]any)
		if !ok {
			add(codeInvalidDocument, `"variables" must be an object`, "/variables")
		} else {
			names := make([]string, 0, len(decls))
			for name := range decls {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				entryPath := "/variables/" + pointerEscape(name)
				if name == "" {
					add(codeInvalidDocument, "variable names must not be empty", entryPath)
					continue
				}
				env.declared[name] = true
				entry, ok := decls[name].(map[string]any)
				if !ok {
					add(codeInvalidDocument, "variable declaration must be an object", entryPath)
					declaredBroken[name] = true
					continue
				}
				keys := make([]string, 0, len(entry))
				for key := range entry {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				for _, key := range keys {
					if key != "schema" && key != "default" {
						add(codeInvalidDocument, fmt.Sprintf("unknown field %q", key), entryPath+"/"+pointerEscape(key))
					}
				}
				schemaRaw, hasSchema := entry["schema"]
				schemaValid := false
				if !hasSchema {
					add(codeInvalidDocument, `missing required field "schema"`, entryPath)
				} else {
					before := len(errs)
					validateSchema(schemaRaw, entryPath+"/schema", add)
					schemaValid = len(errs) == before
				}
				if !schemaValid {
					declaredBroken[name] = true
					continue
				}
				schemas[name] = schemaRaw
				if def, hasDefault := entry["default"]; hasDefault {
					defaults[name] = def
					validateValue(def, schemaRaw, entryPath+"/default", add)
				}
			}
		}
	}

	explicit := map[string]any{}
	if raw, present := root["variableValues"]; present {
		vals, ok := raw.(map[string]any)
		if !ok {
			add(codeInvalidDocument, `"variableValues" must be an object`, "/variableValues")
		} else {
			names := make([]string, 0, len(vals))
			for name := range vals {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				valuePath := "/variableValues/" + pointerEscape(name)
				schema, usable := schemas[name]
				if !usable {
					if !env.declared[name] && !declaredBroken[name] {
						add(codeUnknownVariable, fmt.Sprintf("unknown variable %q", name), valuePath)
					}
					continue
				}
				explicit[name] = vals[name]
				validateValue(vals[name], schema, valuePath, add)
			}
		}
	}

	// Effective values: explicit values win over defaults. Both are taken
	// literally; reference-shaped objects inside them are never resolved.
	for name := range schemas {
		if value, ok := explicit[name]; ok {
			env.values[name] = value
		} else if def, ok := defaults[name]; ok {
			env.values[name] = def
		}
	}
	return env, errs
}

// resolveVariableReferences walks a resource properties tree and replaces
// every variable reference with its effective value. A reference is an
// object whose only field is "$variable" with a non-empty string value; the
// whole object is replaced by the referenced JSON value, never interpolated
// into a string. Values are inserted literally: reference-shaped objects
// inside them are left untouched. It returns ok=false when any reference is
// malformed, unknown, or has no value; findings point at the reference,
// except missing_variable_value, which points at the declaration.
func resolveVariableReferences(value any, env *variableEnv, path string, add func(code, message, path string)) (any, bool) {
	switch v := value.(type) {
	case map[string]any:
		if raw, isReference := v["$variable"]; isReference {
			if len(v) != 1 {
				add(codeInvalidVariableReference, `variable reference object must contain only the "$variable" field`, path)
				return nil, false
			}
			name, isString := raw.(string)
			if !isString || name == "" {
				add(codeInvalidVariableReference, `"$variable" must be a non-empty string`, path)
				return nil, false
			}
			if !env.declared[name] {
				add(codeUnknownVariable, fmt.Sprintf("unknown variable %q", name), path)
				return nil, false
			}
			resolved, hasValue := env.values[name]
			if !hasValue {
				if !env.missing[name] {
					env.missing[name] = true
					add(codeMissingVariableValue, fmt.Sprintf("variable %q has no explicit value or default", name), "/variables/"+pointerEscape(name))
				}
				return nil, false
			}
			return resolved, true
		}
		names := make([]string, 0, len(v))
		for name := range v {
			names = append(names, name)
		}
		sort.Strings(names)
		out := make(map[string]any, len(v))
		for _, name := range names {
			child, ok := resolveVariableReferences(v[name], env, path+"/"+pointerEscape(name), add)
			if !ok {
				return nil, false
			}
			out[name] = child
		}
		return out, true
	case []any:
		out := make([]any, len(v))
		for i, el := range v {
			child, ok := resolveVariableReferences(el, env, fmt.Sprintf("%s/%d", path, i), add)
			if !ok {
				return nil, false
			}
			out[i] = child
		}
		return out, true
	default:
		return value, true
	}
}

// resolveConfigurationVariables returns the view of an already-validated
// configuration document that planning and execution use: resource
// properties with every variable reference replaced by its effective value.
// The variable sections themselves are dropped so downstream snapshots and
// state never echo them. A document without the variable fields is returned
// unchanged.
func resolveConfigurationVariables(doc any) any {
	root, ok := doc.(map[string]any)
	if !ok || !variablesActivated(root) {
		return doc
	}
	env, errs := analyzeVariables(root)
	if len(errs) != 0 {
		// Callers only resolve documents that passed validation.
		return doc
	}
	resources, ok := root["resources"].([]any)
	if !ok {
		return doc
	}
	discard := func(string, string, string) {}
	resolved := make([]any, len(resources))
	for i, entry := range resources {
		obj, ok := entry.(map[string]any)
		if !ok {
			resolved[i] = entry
			continue
		}
		cp := make(map[string]any, len(obj))
		for key, value := range obj {
			cp[key] = value
		}
		if props, ok := obj["properties"].(map[string]any); ok {
			if out, ok := resolveVariableReferences(props, env, "", discard); ok {
				cp["properties"] = out
			}
		}
		resolved[i] = cp
	}
	return map[string]any{"resources": resolved}
}
