package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cliConfigDoc = `{
  "resourceTypes": [{"name": "t", "schema": {"type": "object", "additionalProperties": true}}],
  "resources": [
    {"address": "a", "type": "t", "properties": {}, "dependsOn": ["b"]},
    {"address": "b", "type": "t", "properties": {}}
  ]
}`

const cliPlanDoc = `{
  "configuration": {
    "resourceTypes": [{"name": "t", "schema": {"type": "object", "additionalProperties": true}}],
    "resources": [
      {"address": "a", "type": "t", "properties": {"v": 1}}
    ]
  },
  "priorState": {"resources": []}
}`

// httpBody is the exact body the corresponding endpoint returns for body.
func httpBody(t *testing.T, endpoint, body string) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body)))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	return rec.Body.Bytes()
}

func runCLI(t *testing.T, args []string, stdin string) (int, []byte, []byte) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunCLI(args, strings.NewReader(stdin), &stdout, &stderr)
	return code, stdout.Bytes(), stderr.Bytes()
}

func TestCLIEndpointParity(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		body     string
	}{
		{"validate", "/v1/configurations/validate", cliConfigDoc},
		{"order", "/v1/configurations/order", cliConfigDoc},
		{"plan", "/v1/plans", cliPlanDoc},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, []string{tc.name}, tc.body)
			if code != ExitCLIOK {
				t.Fatalf("exit = %d, want 0; stderr %s", code, stderr)
			}
			if len(stderr) != 0 {
				t.Fatalf("stderr = %q, want empty", stderr)
			}
			want := httpBody(t, tc.endpoint, tc.body)
			if !bytes.Equal(stdout, want) {
				t.Fatalf("stdout = %q, want %q", stdout, want)
			}
			if len(stdout) == 0 || stdout[len(stdout)-1] != '\n' || bytes.Count(stdout, []byte("\n")) != 1 {
				t.Fatalf("stdout must be exactly one JSON line ending in newline: %q", stdout)
			}
		})
	}
}

func TestCLIInvalidJSONParity(t *testing.T) {
	bodies := []string{"", "   ", "{", `{"resourceTypes":`, cliConfigDoc + "\n[]"}
	for _, body := range bodies {
		for _, sub := range []string{"validate", "order", "plan"} {
			endpoint := cliCommands[sub].endpoint
			code, stdout, stderr := runCLI(t, []string{sub}, body)
			if code != ExitCLIUsage {
				t.Fatalf("sub=%s body=%q exit = %d, want 2", sub, body, code)
			}
			if len(stderr) != 0 {
				t.Fatalf("sub=%s body=%q stderr = %q, want empty", sub, body, stderr)
			}
			want := httpBody(t, endpoint, body)
			if !bytes.Equal(stdout, want) {
				t.Fatalf("sub=%s body=%q stdout = %q, want %q", sub, body, stdout, want)
			}
			var res validateResponse
			if err := json.Unmarshal(stdout, &res); err != nil {
				t.Fatalf("sub=%s body=%q output is not JSON: %v", sub, body, err)
			}
			if res.Valid || len(res.Errors) != 1 || res.Errors[0].Code != codeInvalidJSON {
				t.Fatalf("sub=%s body=%q unexpected payload %v", sub, body, res)
			}
		}
	}
}

func TestCLIValidationErrorParity(t *testing.T) {
	badConfig := `{"resourceTypes": [], "resources": [{"address": "a", "type": "nope", "properties": {}}]}`
	cases := []struct {
		name     string
		endpoint string
		body     string
	}{
		{"validate", "/v1/configurations/validate", badConfig},
		{"order", "/v1/configurations/order", badConfig},
		{"plan", "/v1/plans", `{"configuration": ` + badConfig + `, "priorState": {"resources": []}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, []string{tc.name}, tc.body)
			if code != ExitCLIUsage {
				t.Fatalf("exit = %d, want 2; stderr %s", code, stderr)
			}
			if len(stderr) != 0 {
				t.Fatalf("stderr = %q, want empty", stderr)
			}
			want := httpBody(t, tc.endpoint, tc.body)
			if !bytes.Equal(stdout, want) {
				t.Fatalf("stdout = %q, want %q", stdout, want)
			}
		})
	}
}

func TestCLIPlanNumericAndSetSemantics(t *testing.T) {
	// Numeric equivalence (1 vs 1.0) and dependency set semantics must
	// produce the same noop/change answer as the HTTP entry.
	body := `{
	  "configuration": {
	    "resourceTypes": [{"name": "t", "schema": {"type": "object", "additionalProperties": true}}],
	    "resources": [
	      {"address": "a", "type": "t", "properties": {"v": 1}, "dependsOn": ["b"]},
	      {"address": "b", "type": "t", "properties": {}}
	    ]
	  },
	  "priorState": {"resources": [
	    {"address": "a", "type": "t", "properties": {"v": 1.0}, "dependsOn": ["b"]},
	    {"address": "b", "type": "t", "properties": {}}
	  ]}
	}`
	code, stdout, stderr := runCLI(t, []string{"plan"}, body)
	if code != ExitCLIOK {
		t.Fatalf("exit = %d, want 0; stderr %s", code, stderr)
	}
	want := httpBody(t, "/v1/plans", body)
	if !bytes.Equal(stdout, want) {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	var res planResponse
	if err := json.Unmarshal(stdout, &res); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(res.Changes) != 0 || res.Summary.Noop != 2 {
		t.Fatalf("expected two noops, got changes=%v summary=%+v", res.Changes, res.Summary)
	}
}

func TestCLIArgumentErrors(t *testing.T) {
	cases := [][]string{
		{"bogus"},
		{"validate", "extra"},
		{"validate", "--unknown"},
		{"validate", "--file"},
		{"validate", "--file="},
		{"validate", "--file", "--unknown"},
		{"validate", "--file", "--file", "b.json"},
		{"validate", "--file", "a.json", "--file", "b.json"},
		{"validate", "--file=a.json", "--file=b.json"},
		{"order", "positional", "--file", "a.json"},
	}
	for _, args := range cases {
		code, stdout, stderr := runCLI(t, args, cliConfigDoc)
		if code != ExitCLIUsage {
			t.Fatalf("args=%v exit = %d, want 2", args, code)
		}
		if len(stdout) != 0 {
			t.Fatalf("args=%v stdout = %q, want empty", args, stdout)
		}
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(stderr), &envelope); err != nil {
			t.Fatalf("args=%v stderr %q is not JSON: %v", args, stderr, err)
		}
		if envelope.Error.Code != "invalid_cli_arguments" {
			t.Fatalf("args=%v code = %q, want invalid_cli_arguments", args, envelope.Error.Code)
		}
		if envelope.Error.Message == "" {
			t.Fatalf("args=%v empty error message", args)
		}
		if bytes.Count(stderr, []byte("\n")) != 1 || !bytes.HasSuffix(stderr, []byte("\n")) {
			t.Fatalf("args=%v stderr must be one newline-terminated line: %q", args, stderr)
		}
	}
}

func TestCLINoArgs(t *testing.T) {
	code, stdout, stderr := runCLI(t, nil, "")
	if code != ExitCLIUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
	if len(stdout) != 0 {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !bytes.Contains(stderr, []byte("invalid_cli_arguments")) {
		t.Fatalf("stderr = %q, want invalid_cli_arguments", stderr)
	}
}

func TestCLIFileInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(cliConfigDoc), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{{"validate", "--file", path}, {"validate", "--file=" + path}} {
		code, stdout, stderr := runCLI(t, args, "{}")
		if code != ExitCLIOK {
			t.Fatalf("args=%v exit = %d, want 0; stderr %s", args, code, stderr)
		}
		want := httpBody(t, "/v1/configurations/validate", cliConfigDoc)
		if !bytes.Equal(stdout, want) {
			t.Fatalf("args=%v stdout = %q, want %q", args, stdout, want)
		}
	}
}

func TestCLIFileMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.json")
	for _, args := range [][]string{{"plan", "--file", missing}, {"plan", "--file=" + missing}} {
		code, stdout, stderr := runCLI(t, args, cliPlanDoc)
		if code != ExitCLIInputRead {
			t.Fatalf("args=%v exit = %d, want 1", args, code)
		}
		if len(stdout) != 0 {
			t.Fatalf("args=%v stdout = %q, want empty", args, stdout)
		}
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(stderr, &envelope); err != nil {
			t.Fatalf("args=%v stderr %q is not JSON: %v", args, stderr, err)
		}
		if envelope.Error.Code != "input_read_error" {
			t.Fatalf("args=%v code = %q, want input_read_error", args, envelope.Error.Code)
		}
	}
}

func TestCLIFileUnreadableDirectory(t *testing.T) {
	// Opening a directory succeeds; reading it fails, which must also be
	// input_read_error with no fallback to stdin.
	dir := t.TempDir()
	code, stdout, stderr := runCLI(t, []string{"validate", "--file", dir}, cliConfigDoc)
	if code != ExitCLIInputRead {
		t.Fatalf("exit = %d, want 1; stderr %s", code, stderr)
	}
	if len(stdout) != 0 {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !bytes.Contains(stderr, []byte("input_read_error")) {
		t.Fatalf("stderr = %q, want input_read_error", stderr)
	}
}

func TestCLINeverTouchesServerState(t *testing.T) {
	// A pointed STACKWEAVER_ADDR must not affect the result; the command
	// neither binds nor connects. The real guarantee is structural (RunCLI
	// never references the environment), but pin the observable behavior.
	t.Setenv("STACKWEAVER_ADDR", "127.0.0.1:1")
	code, stdout, stderr := runCLI(t, []string{"order"}, cliConfigDoc)
	if code != ExitCLIOK || len(stderr) != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	want := httpBody(t, "/v1/configurations/order", cliConfigDoc)
	if !bytes.Equal(stdout, want) {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}
