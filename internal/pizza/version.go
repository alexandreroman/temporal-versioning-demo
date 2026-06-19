package pizza

// Version identifies a workflow shape. The worker selects one at startup via the
// PIZZA_VERSION env var; the value is reported back through the getState query so
// the UI can colour orders without decoding Build IDs.
type Version string

// Known workflow shapes.
const (
	V1 Version = "v1"
	V2 Version = "v2"
	V3 Version = "v3"
)

// ParseVersion validates a PIZZA_VERSION value.
func ParseVersion(s string) (Version, bool) {
	switch Version(s) {
	case V1, V2, V3:
		return Version(s), true
	default:
		return "", false
	}
}
