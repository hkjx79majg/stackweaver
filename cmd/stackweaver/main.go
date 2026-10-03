package main

import (
	"errors"
	"log"
	"net/http"
	"os"

	"github.com/hkjx79majg/stackweaver/internal/server"
)

func main() {
	if len(os.Args) > 1 {
		// Read-only local pipeline: no listener, no Provider, no state
		// file, and STACKWEAVER_ADDR is intentionally ignored.
		os.Exit(server.RunCLI(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}

	addr := os.Getenv("STACKWEAVER_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	log.Printf("StackWeaver listening on %s", addr)
	if err := http.ListenAndServe(addr, server.Handler()); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
