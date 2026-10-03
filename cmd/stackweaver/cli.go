package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/hkjx79majg/stackweaver/internal/server"
)

// cliCommands maps each read-only subcommand to the HTTP entry point whose
// request document and response object it shares.
var cliCommands = map[string]string{
	"validate": "/v1/configurations/validate",
	"order":    "/v1/configurations/order",
	"plan":     "/v1/plans",
}

const (
	cliExitOK             = 0
	cliExitInputReadError = 1
	cliExitClientError    = 2
)

// runCLI executes a read-only subcommand without starting a listener,
// touching a Provider, or reading or writing any state file. Business output
// (success objects, invalid_json and validation errors) is written to stdout
// exactly as the corresponding HTTP entry point produces it; command-line and
// input-file failures are single-line JSON objects on stderr. It returns the
// process exit code: 0 on success, 1 when the input source cannot be read,
// and 2 for argument errors or invalid input documents.
func runCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return writeCLIError(stderr, "invalid_cli_arguments", "missing subcommand: expected one of validate, order, plan")
	}
	path, known := cliCommands[args[0]]
	if !known {
		return writeCLIError(stderr, "invalid_cli_arguments", fmt.Sprintf("unknown subcommand %q: expected one of validate, order, plan", args[0]))
	}

	var file string
	fileSeen := false
	pendingFileValue := false
	for _, arg := range args[1:] {
		switch {
		case pendingFileValue && !isCLIOption(arg):
			file = arg
			pendingFileValue = false
		case arg == "--file":
			if fileSeen || pendingFileValue {
				return writeCLIError(stderr, "invalid_cli_arguments", `flag "--file" must not be repeated`)
			}
			fileSeen = true
			pendingFileValue = true
		case strings.HasPrefix(arg, "--file="):
			if fileSeen || pendingFileValue {
				return writeCLIError(stderr, "invalid_cli_arguments", `flag "--file" must not be repeated`)
			}
			value := strings.TrimPrefix(arg, "--file=")
			if value == "" {
				return writeCLIError(stderr, "invalid_cli_arguments", `flag "--file" requires a path argument`)
			}
			fileSeen = true
			file = value
		case isCLIOption(arg):
			return writeCLIError(stderr, "invalid_cli_arguments", fmt.Sprintf("unknown option %q: only --file PATH is supported", arg))
		default:
			return writeCLIError(stderr, "invalid_cli_arguments", fmt.Sprintf("unexpected positional argument %q", arg))
		}
	}
	if pendingFileValue {
		return writeCLIError(stderr, "invalid_cli_arguments", `flag "--file" requires a path argument`)
	}

	var input []byte
	if file != "" {
		// A named input source is authoritative: a read failure is reported
		// outright, never by falling back to standard input.
		data, err := os.ReadFile(file)
		if err != nil {
			return writeCLIError(stderr, "input_read_error", fmt.Sprintf("cannot read input file %q: %v", file, err))
		}
		input = data
	} else {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return writeCLIError(stderr, "input_read_error", fmt.Sprintf("cannot read standard input: %v", err))
		}
		input = data
	}

	// Dispatch through the frozen HTTP surface in-process: no port is opened,
	// and the read-only handlers never consult a Provider or the state file.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(input))
	server.Handler().ServeHTTP(rec, req)

	// Every handler writes exactly one JSON-encoded line with a trailing
	// newline, matching the on-wire HTTP response body byte for byte.
	_, _ = stdout.Write(rec.Body.Bytes())
	if rec.Code == http.StatusOK {
		return cliExitOK
	}
	return cliExitClientError
}

// isCLIOption reports whether token looks like a flag rather than the path
// value of --file.
func isCLIOption(token string) bool {
	return len(token) > 0 && token[0] == '-'
}

// writeCLIError emits one single-line JSON error object to the error stream
// and returns the exit code associated with the error kind.
func writeCLIError(stderr io.Writer, code, message string) int {
	encoded, err := json.Marshal(map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
	if err != nil {
		// json.Marshal on this fixed shape cannot fail; keep a safe fallback.
		encoded = []byte(`{"error":{"code":"` + code + `","message":"failed to encode error"}}`)
	}
	encoded = append(encoded, '\n')
	_, _ = stderr.Write(encoded)
	if code == "input_read_error" {
		return cliExitInputReadError
	}
	return cliExitClientError
}
