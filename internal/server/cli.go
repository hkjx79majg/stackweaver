package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
)

// CLI exit codes shared by the stackweaver command line.
const (
	// ExitCLIOK marks a successful validate, order, or plan run.
	ExitCLIOK = 0
	// ExitCLIInputRead marks a failure reading the requested input file.
	ExitCLIInputRead = 1
	// ExitCLIUsage marks a command-line argument error or a rejected
	// document (invalid JSON or failed business validation).
	ExitCLIUsage = 2
)

// cliError is the single-line stderr envelope for command-line problems.
type cliError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// cliCommand is one read-only subcommand and the HTTP endpoint whose
// response semantics it shares.
type cliCommand struct {
	name     string
	endpoint string
}

var cliCommands = map[string]cliCommand{
	"validate": {name: "validate", endpoint: "/v1/configurations/validate"},
	"order":    {name: "order", endpoint: "/v1/configurations/order"},
	"plan":     {name: "plan", endpoint: "/v1/plans"},
}

// RunCLI executes a read-only subcommand without starting a server. Input is
// read from stdin, or from a single file with --file. On success it writes
// exactly the endpoint's response body and returns 0. Invalid JSON or failed
// validation writes the endpoint's 400/422 body to stdout and returns 2.
// Command-line argument errors write a single-line invalid_cli_arguments
// object to stderr (stdout stays empty) and return 2; an unreadable input
// file does the same with input_read_error and returns 1, never falling back
// to stdin. It never starts a listener, calls a Provider, reads a state file,
// or writes anything besides stdout and stderr.
func RunCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return writeCLIError(stderr, "invalid_cli_arguments", "a subcommand is required: validate, order, or plan")
	}
	cmd, known := cliCommands[args[0]]
	if !known {
		return writeCLIError(stderr, "invalid_cli_arguments", "unknown subcommand "+jsonString(args[0])+": expected validate, order, or plan")
	}

	var file string
	fileSet := false
	awaitingValue := false
	for _, arg := range args[1:] {
		switch {
		case awaitingValue:
			if strings.HasPrefix(arg, "-") {
				return writeCLIError(stderr, "invalid_cli_arguments", `option "--file" requires a value`)
			}
			file = arg
			awaitingValue = false
		case arg == "--file":
			if fileSet {
				return writeCLIError(stderr, "invalid_cli_arguments", `duplicate option "--file"`)
			}
			fileSet = true
			awaitingValue = true
		case bytes.HasPrefix([]byte(arg), []byte("--file=")):
			value := arg[len("--file="):]
			if fileSet {
				return writeCLIError(stderr, "invalid_cli_arguments", `duplicate option "--file"`)
			}
			if value == "" {
				return writeCLIError(stderr, "invalid_cli_arguments", `option "--file" requires a value`)
			}
			file = value
			fileSet = true
		default:
			return writeCLIError(stderr, "invalid_cli_arguments", "unexpected argument "+jsonString(arg))
		}
	}
	if awaitingValue {
		return writeCLIError(stderr, "invalid_cli_arguments", `option "--file" requires a value`)
	}

	input := stdin
	if file != "" {
		f, err := os.Open(file)
		if err != nil {
			return writeCLIError(stderr, "input_read_error", "cannot read input file "+jsonString(file)+": "+err.Error())
		}
		defer f.Close()
		input = f
	}

	body, err := io.ReadAll(input)
	if err != nil {
		return writeCLIError(stderr, "input_read_error", "failed reading input: "+err.Error())
	}

	// Dispatch through the real HTTP handler so the command shares exactly
	// the decode, validation, ordering, and plan behavior of the endpoint.
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, cmd.endpoint, bytes.NewReader(body)))

	_, _ = stdout.Write(rec.Body.Bytes())
	if rec.Code == http.StatusOK {
		return ExitCLIOK
	}
	return ExitCLIUsage
}

// writeCLIError writes one JSON object followed by a newline as the single
// stderr line and returns the associated exit code.
func writeCLIError(w io.Writer, code, message string) int {
	var e cliError
	e.Error.Code = code
	e.Error.Message = message
	line, _ := json.Marshal(e)
	line = append(line, '\n')
	_, _ = w.Write(line)
	if code == "input_read_error" {
		return ExitCLIInputRead
	}
	return ExitCLIUsage
}

// jsonString renders s as a compact JSON string literal for messages.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
