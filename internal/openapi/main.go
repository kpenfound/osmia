// Command openapi writes the OpenAPI description of the local service API,
// built from service.Routes and the Go types of each request and response.
//
// Usage: go run ./internal/openapi <output>
package main

//go:generate go run . ../../docs/openapi.json
//go:generate:include ../../docs/openapi.json

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: openapi <output>")
		os.Exit(2)
	}
	spec, err := Spec()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[1], spec, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
