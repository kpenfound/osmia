package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"

	"github.com/swaggest/jsonschema-go"
	"github.com/swaggest/openapi-go"
	"github.com/swaggest/openapi-go/openapi31"

	"github.com/kpenfound/osmia/internal/service"
)

const description = `The Osmia service serves this API over its Unix socket and, when configured,
on a loopback address (listen.web) and on the tailnet (listen.tailnet). There is
no authentication: the socket, the loopback interface and tailnet membership are
the boundaries. On the web and tailnet listeners, requests must carry a Host
naming the listener, and requests other than GET and HEAD must send
Content-Type: application/json.

Request bodies are one JSON object with known, unique fields. Every failure
returns an error object with a code and a message.`

const streamDescription = `A text/event-stream of frames "event: <kind>" with a JSON data line
holding the event. The first event is resync, which asks the client to read
every view again; it also replaces events a slow subscriber missed. A comment
line keeps an idle stream open.`

// parameters describes each path parameter the routes use.
var parameters = map[string]string{
	"workstream": "Workstream ID: w_ followed by 32 lowercase hexadecimal digits.",
	"project":    "Project ID.",
	"number":     "Inbox entry number.",
	"unit":       "Unit ID from the workstream's plan.",
	"amendment":  "Amendment number within the workstream.",
	"question":   "Question number whose ruling the charter proposal comes from.",
	"criterion":  "Criterion cited as spec#<n>.",
	"commit":     "Full lowercase commit ID.",
}

var placeholder = regexp.MustCompile(`\{([a-z]+)\}`)

// Spec returns the indented OpenAPI 3.1 description of service.Routes.
func Spec() ([]byte, error) {
	r := openapi31.NewReflector()
	r.Spec.Info.WithTitle("Osmia local API").WithVersion("1").WithDescription(description)
	host := "The listen.web address, such as localhost:8080, or the listen.tailnet hostname."
	r.Spec.WithServers(openapi31.Server{URL: "http://{host}", Variables: map[string]openapi31.ServerVariable{
		"host": {Default: "osmia", Description: &host},
	}})
	r.JSONSchemaReflector().DefaultOptions = append(r.JSONSchemaReflector().DefaultOptions,
		jsonschema.InterceptDefName(func(_ reflect.Type, name string) string {
			name = strings.TrimPrefix(name, "Service")
			// A package's type named after it, such as config.Config.
			if half := len(name) / 2; len(name)%2 == 0 && name[:half] == name[half:] {
				return name[:half]
			}
			return name
		}))
	for _, route := range service.Routes {
		oc, err := r.NewOperationContext(route.Method, route.Path)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", route.Method, route.Path, err)
		}
		oc.SetID(operationID(route))
		oc.SetSummary(route.Summary)
		if params := pathParameters(route.Path); params != nil {
			oc.AddReqStructure(params)
		}
		if route.Request != nil && route.Method != http.MethodDelete {
			oc.AddReqStructure(route.Request)
		}
		status := route.Status
		if status == 0 {
			status = http.StatusOK
		}
		if route.Stream {
			oc.SetDescription(streamDescription)
			oc.AddRespStructure(route.Response, openapi.WithHTTPStatus(status), openapi.WithContentType("text/event-stream"))
		} else {
			oc.AddRespStructure(route.Response, openapi.WithHTTPStatus(status))
		}
		if status < 400 {
			oc.AddRespStructure(service.ErrorResponse{}, func(cu *openapi.ContentUnit) {
				cu.IsDefault = true
				cu.Description = "The request failed; the error names its code and reason."
			})
		}
		if err := r.AddOperation(oc); err != nil {
			return nil, fmt.Errorf("%s %s: %w", route.Method, route.Path, err)
		}
		if route.Request != nil && route.Method == http.MethodDelete {
			if err := deleteBody(r, route); err != nil {
				return nil, err
			}
		}
	}
	out, err := json.MarshalIndent(r.Spec, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// deleteBody attaches route's request body to its DELETE operation.
// openapi-go reflects bodies only for methods that usually carry one, so the
// body is reflected on a temporary POST operation and moved.
func deleteBody(r *openapi31.Reflector, route service.Route) error {
	scratch := "/delete-body" + route.Path
	oc, err := r.NewOperationContext(http.MethodPost, scratch)
	if err != nil {
		return err
	}
	if params := pathParameters(route.Path); params != nil {
		oc.AddReqStructure(params)
	}
	oc.AddReqStructure(route.Request)
	if err := r.AddOperation(oc); err != nil {
		return fmt.Errorf("%s %s: %w", route.Method, route.Path, err)
	}
	paths := r.Spec.Paths.MapOfPathItemValues
	paths[route.Path].Delete.RequestBody = paths[scratch].Post.RequestBody
	delete(paths, scratch)
	return nil
}

// operationID names route after its method and path, such as
// getStatusByWorkstream for GET /v1/status/{workstream}.
func operationID(route service.Route) string {
	id := strings.ToLower(route.Method)
	for _, segment := range strings.Split(strings.TrimPrefix(route.Path, service.Prefix+"/"), "/") {
		if name, ok := strings.CutPrefix(segment, "{"); ok {
			segment = "by-" + strings.TrimSuffix(name, "}")
		}
		for _, word := range strings.Split(segment, "-") {
			id += strings.ToUpper(word[:1]) + word[1:]
		}
	}
	return id
}

// pathParameters returns a zero value of a struct with one string field per
// placeholder in path, tagged for openapi-go, or nil when path has none.
func pathParameters(path string) any {
	var fields []reflect.StructField
	for _, m := range placeholder.FindAllStringSubmatch(path, -1) {
		name := m[1]
		fields = append(fields, reflect.StructField{
			Name: strings.ToUpper(name[:1]) + name[1:],
			Type: reflect.TypeOf(""),
			Tag:  reflect.StructTag(fmt.Sprintf(`path:%q description:%q`, name, parameters[name])),
		})
	}
	if fields == nil {
		return nil
	}
	return reflect.New(reflect.StructOf(fields)).Elem().Interface()
}
