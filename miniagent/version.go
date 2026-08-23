package miniagent

// Version identifies this agent build in outbound requests (User-Agent "miniagent/{Version}",
// -version banner, /api/whoami). Default "dev"; overridden at build time via make build's
// -ldflags "-X github.com/justphantom/miniagent/miniagent.Version=$(git describe --tags)".
var Version = "v6.6.2"

// UserAgent returns the outbound User-Agent for all requests this agent makes.
func UserAgent() string { return "miniagent/" + Version }
