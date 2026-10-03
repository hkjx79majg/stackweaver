package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hkjx79majg/stackweaver/internal/server"
)

// cliResult captures one runCLI invocation.
type cliResult struct {
	code   int
	stdout string
	stderr string
}

func runCLIWith(t *testing.T, args []string, stdin string) cliResult {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := runCLI(args, strings.NewReader(stdin), &out, &errBuf)
	return cliResult{code: code, stdout: out.String(), stderr: errBuf.String()}
}

// httpBody returns the exact body the HTTP entry point produces for a command
// and document, so CLI tests can assert byte-for-byte equivalence.
func httpBody(t *testing.T, command, body string) (int, string) {
	t.Helper()
	path := cliCommands[command]
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec.Code, rec.Body.String()
}

const validConfig = `{
  "resourceTypes": [
    {"name": "net", "schema": {"type": "object", "properties": {"cidr": {"type": "string"}}, "required": ["cidr"]}}
  ],
  "resources": [
    {"address": "net.a", "type": "net", "properties": {"cidr": "10.0.0.0/24"}, "dependsOn": []},
    {"address": "net.b", "type": "net", "properties": {"cidr": "10.0.1.0/24"}, "dependsOn": ["net.a"]}
  ]
}`

const planRequest = `{
  "configuration": {
    "resourceTypes": [{"name": "net", "schema": {"type": "object", "properties": {"cidr": {"type": "string"}}, "required": ["cidr"]}}],
    "resources": [
      {"address": "net.b", "type": "net", "properties": {"cidr": "10.0.1.0/24"}},
      {"address": "net.a", "type": "net", "properties": {"cidr": "10.0.0.0/24"}}
    ]
  },
  "priorState": {"resources": []}
}`

func TestCLISuccessMatchesHTTP(t *testing.T) {
	cases := []struct {
		command string
		body    string
	}{
		{"validate", validConfig},
		{"order", validConfig},
		{"plan", planRequest},
	}
	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			status, expected := httpBody(t, tc.command, tc.body)
			if status != http.StatusOK {
				t.Fatalf("test fixture: HTTP status = %d, want 200", status)
			}
			res := runCLIWith(t, []string{tc.command}, tc.body)
			if res.code != 0 {
				t.Fatalf("exit = %d, want 0; stderr=%s", res.code, res.stderr)
			}
			if res.stdout != expected {
				t.Fatalf("stdout = %q, want HTTP body %q", res.stdout, expected)
			}
			if res.stderr != "" {
				t.Fatalf("stderr = %q, want empty", res.stderr)
			}
			if !strings.HasSuffix(res.stdout, "\n") {
				t.Fatalf("stdout must end with a newline: %q", res.stdout)
			}
			assertSingleJSONValue(t, res.stdout)
		})
	}
}

func TestCLIOrderReflectsTopologicalOrder(t *testing.T) {
	res := runCLIWith(t, []string{"order"}, validConfig)
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr=%s", res.code, res.stderr)
	}
	var payload struct {
		Valid bool     `json:"valid"`
		Order []string `json:"order"`
	}
	if err := json.Unmarshal([]byte(res.stdout), &payload); err != nil {
		t.Fatalf("invalid output JSON: %v (%q)", err, res.stdout)
	}
	if !payload.Valid || len(payload.Order) != 2 || payload.Order[0] != "net.a" || payload.Order[1] != "net.b" {
		t.Fatalf("unexpected order payload: %s", res.stdout)
	}
}

func TestCLIPlanMatchesHTTPAcrossSuccessAndFailure(t *testing.T) {
	// A document exercising creates, updates (numeric-equivalence noop vs
	// real change), deletes and dependency-set semantics.
	body := `{
	  "configuration": {
	    "resourceTypes": [{"name": "t", "schema": {"type": "object", "additionalProperties": true}}],
	    "resources": [
	      {"address": "a", "type": "t", "properties": {"v": 1}, "dependsOn": ["b"]},
	      {"address": "b", "type": "t", "properties": {"v": 2}},
	      {"address": "c", "type": "t", "properties": {"v": 3}}
	    ]
	  },
	  "priorState": {"resources": [
	    {"address": "a", "type": "t", "properties": {"v": 1.0}, "dependsOn": ["b"]},
	    {"address": "b", "type": "t", "properties": {"v": 20}},
	    {"address": "gone", "type": "t", "properties": {}}
	  ]}
	}`
	status, expected := httpBody(t, "plan", body)
	if status != http.StatusOK {
		t.Fatalf("test fixture status = %d", status)
	}
	res := runCLIWith(t, []string{"plan"}, body)
	if res.code != 0 || res.stdout != expected {
		t.Fatalf("exit=%d stdout=%q want %q stderr=%s", res.code, res.stdout, expected, res.stderr)
	}
	var payload struct {
		Summary struct {
			Create int `json:"create"`
			Update int `json:"update"`
			Delete int `json:"delete"`
			Noop   int `json:"noop"`
		} `json:"summary"`
	}
	_ = json.Unmarshal([]byte(res.stdout), &payload)
	if payload.Summary.Create != 1 || payload.Summary.Update != 1 ||
		payload.Summary.Delete != 1 || payload.Summary.Noop != 1 {
		t.Fatalf("unexpected summary in %s", res.stdout)
	}
}

func TestCLIInvalidJSONMatchesHTTP(t *testing.T) {
	cases := map[string]string{
		"empty":      "",
		"whitespace": "   \n\t ",
		"malformed":  `{"resourceTypes": [`,
		"trailing":   `{"resourceTypes": [], "resources": []} {"more": true}`,
		"twoValues":  `true false`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			for _, command := range []string{"validate", "order", "plan"} {
				status, expected := httpBody(t, command, body)
				if status != http.StatusBadRequest {
					t.Fatalf("fixture: %s HTTP status = %d, want 400", command, status)
				}
				res := runCLIWith(t, []string{command}, body)
				if res.code != 2 {
					t.Fatalf("%s: exit = %d, want 2", command, res.code)
				}
				if res.stdout != expected {
					t.Fatalf("%s: stdout = %q, want %q", command, res.stdout, expected)
				}
				if res.stderr != "" {
					t.Fatalf("%s: stderr = %q, want empty", command, res.stderr)
				}
				assertSingleJSONValue(t, res.stdout)
				if !strings.Contains(res.stdout, `"invalid_json"`) {
					t.Fatalf("%s: expected invalid_json in %q", command, res.stdout)
				}
			}
		})
	}
}

func TestCLIValidationErrorsMatchHTTP(t *testing.T) {
	// Unknown type + missing field: business validation failure (422).
	body := `{"resourceTypes": [], "resources": [{"address": "a", "type": "ghost", "properties": {}}]}`
	for _, command := range []string{"validate", "order"} {
		status, expected := httpBody(t, command, body)
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("fixture: %s status = %d, want 422", command, status)
		}
		res := runCLIWith(t, []string{command}, body)
		if res.code != 2 || res.stdout != expected || res.stderr != "" {
			t.Fatalf("%s: code=%d stdout=%q want %q stderr=%q", command, res.code, res.stdout, expected, res.stderr)
		}
	}

	// Dependency cycle surfaces through order but not validate.
	cycle := `{
	  "resourceTypes": [{"name": "t", "schema": {"type": "object", "additionalProperties": true}}],
	  "resources": [
	    {"address": "a", "type": "t", "properties": {}, "dependsOn": ["b"]},
	    {"address": "b", "type": "t", "properties": {}, "dependsOn": ["a"]}
	  ]
	}`
	status, expected := httpBody(t, "order", cycle)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("fixture status = %d, want 422", status)
	}
	res := runCLIWith(t, []string{"order"}, cycle)
	if res.code != 2 || res.stdout != expected {
		t.Fatalf("order cycle: code=%d stdout=%q want %q", res.code, res.stdout, expected)
	}
	if !strings.Contains(res.stdout, "dependency_cycle") {
		t.Fatalf("expected dependency_cycle in %q", res.stdout)
	}

	// Invalid plan envelope maps to the plan endpoint's 422 object.
	badEnvelope := `{"configuration": {"resourceTypes": [], "resources": []}}`
	status, expected = httpBody(t, "plan", badEnvelope)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("fixture plan status = %d, want 422", status)
	}
	res = runCLIWith(t, []string{"plan"}, badEnvelope)
	if res.code != 2 || res.stdout != expected || res.stderr != "" {
		t.Fatalf("plan envelope: code=%d stdout=%q want %q stderr=%q", res.code, res.stdout, expected, res.stderr)
	}
}

func TestCLIArgumentErrors(t *testing.T) {
	cases := [][]string{
		{"frobnicate"},
		{"validate", "extra-positional"},
		{"order", "--bogus"},
		{"plan", "--file"},
		{"validate", "--file="},
		{"validate", "--file", "a.json", "--file", "b.json"},
		{"validate", "--file=a.json", "--file=b.json"},
		{"order", "--file", "a.json", "extra"},
		{"plan", "--unknown", "--file", "a.json"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "|"), func(t *testing.T) {
			res := runCLIWith(t, args, validConfig)
			if res.code != 2 {
				t.Fatalf("exit = %d, want 2", res.code)
			}
			if res.stdout != "" {
				t.Fatalf("stdout = %q, want empty", res.stdout)
			}
			if res.stderr == "" || !strings.HasSuffix(res.stderr, "\n") {
				t.Fatalf("stderr = %q, want one terminated line", res.stderr)
			}
			if strings.Contains(res.stderr, "\n\n") || strings.Count(res.stderr, "\n") != 1 {
				t.Fatalf("stderr must be a single line: %q", res.stderr)
			}
			var payload struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(res.stderr), &payload); err != nil {
				t.Fatalf("stderr is not valid JSON: %v (%q)", err, res.stderr)
			}
			if payload.Error.Code != "invalid_cli_arguments" || payload.Error.Message == "" {
				t.Fatalf("unexpected error object: %q", res.stderr)
			}
		})
	}
}

func TestCLIFileInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, form := range [][]string{{"validate", "--file", path}, {"order", "--file=" + path}} {
		t.Run(strings.Join(form[1:], "|"), func(t *testing.T) {
			// Stdin is deliberately invalid; a --file source must win and the
			// result must still be success.
			res := runCLIWith(t, form, "not json at all")
			if res.code != 0 {
				t.Fatalf("exit = %d, want 0; stderr=%s", res.code, res.stderr)
			}
			_, expected := httpBody(t, form[0], validConfig)
			if res.stdout != expected {
				t.Fatalf("stdout = %q, want %q", res.stdout, expected)
			}
			if res.stderr != "" {
				t.Fatalf("stderr = %q, want empty", res.stderr)
			}
		})
	}
}

func TestCLIFileReadErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.json")
	res := runCLIWith(t, []string{"validate", "--file", missing}, validConfig)
	if res.code != 1 {
		t.Fatalf("missing file: exit = %d, want 1", res.code)
	}
	if res.stdout != "" {
		t.Fatalf("missing file: stdout = %q, want empty", res.stdout)
	}
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(res.stderr), &payload); err != nil {
		t.Fatalf("stderr not JSON: %v (%q)", err, res.stderr)
	}
	if payload.Error.Code != "input_read_error" || payload.Error.Message == "" {
		t.Fatalf("unexpected error: %q", res.stderr)
	}

	// A directory cannot be read as a file either.
	res = runCLIWith(t, []string{"plan", "--file", t.TempDir()}, planRequest)
	if res.code != 1 || res.stdout != "" || !strings.Contains(res.stderr, "input_read_error") {
		t.Fatalf("directory: code=%d stdout=%q stderr=%q", res.code, res.stdout, res.stderr)
	}
}

func TestCLINoFallbackToStdin(t *testing.T) {
	// Even with perfectly valid data on stdin, an unreadable file must fail
	// with input_read_error and never consume stdin.
	missing := filepath.Join(t.TempDir(), "nope.json")
	res := runCLIWith(t, []string{"order", "--file", missing}, validConfig)
	if res.code != 1 {
		t.Fatalf("exit = %d, want 1", res.code)
	}
	if res.stdout != "" || !strings.Contains(res.stderr, "input_read_error") {
		t.Fatalf("stdout=%q stderr=%q", res.stdout, res.stderr)
	}
}

// assertSingleJSONValue fails if text is not exactly one JSON value followed
// by a single trailing newline.
func assertSingleJSONValue(t *testing.T, text string) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(text))
	var first any
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("output is not a JSON value: %v (%q)", err, text)
	}
	var trailing any
	if err := dec.Decode(&trailing); err == nil {
		t.Fatalf("output contains a trailing JSON value: %q", text)
	}
	if !strings.HasSuffix(text, "\n") {
		t.Fatalf("output missing trailing newline: %q", text)
	}
}
