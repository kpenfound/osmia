package telemetry

import (
	"regexp"
	"strings"
)

// Namespace prefixes every span name and attribute key Osmia defines.
const Namespace = "osmia"

var namePart = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Key joins parts into an attribute key under the osmia namespace, such as
// Key("pool", "limit") for "osmia.pool.limit". It returns "" when a part is
// not a lowercase word.
func Key(parts ...string) string {
	return join(parts)
}

func join(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	for _, p := range parts {
		if !namePart.MatchString(p) {
			return ""
		}
	}
	return Namespace + "." + strings.Join(parts, ".")
}

// Name is a span name built by SpanName or GenAIName. Its content cannot be
// set any other way, so a span name never carries free text.
type Name struct{ s string }

// String returns the span name.
func (n Name) String() string { return n.s }

// Valid reports whether n was built by a naming helper.
func (n Name) Valid() bool { return n.s != "" }

// SpanName builds the name of an Osmia span from lowercase words, such as
// SpanName("unit", "state") for "osmia.unit.state". A word that is not
// lowercase gives an invalid Name, and a span with it is dropped.
func SpanName(parts ...string) Name {
	return Name{join(parts)}
}

// GenAIName builds the name of a generative AI span the way the OpenTelemetry
// GenAI conventions do: the operation, a space and what it was applied to, the
// agent's name for an agent invocation or the requested model for an inference.
// The operation must be a lowercase word and the target identifier-shaped;
// otherwise the Name is invalid.
func GenAIName(operation, target string) Name {
	if !namePart.MatchString(operation) || !identifier.MatchString(target) {
		return Name{}
	}
	return Name{operation + " " + target}
}
