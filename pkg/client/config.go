package client

// =============================================================================
// BUILD-TIME SERVER CONFIGURATION
// =============================================================================
//
// The CLI tools target exactly ONE server. There is deliberately no
// user-facing choice of environment: a distributed .exe must never be
// redirectable at a different deployment, because these tools can delete
// tracks and rebuild the catalog. The target is fixed at BUILD time and
// displayed to the operator at startup so it is never ambiguous.
//
// API_BASE_URL remains as a developer escape hatch only — it wins over the
// default so local testing needs no rebuild:
//
//   # local development against a locally running Go server
//   $env:API_BASE_URL = "http://localhost:8080"
//
// Release builds pin the target with -ldflags (see the release command in
// the build notes). Until a production deployment exists, the default below
// IS the staging server, and EnvironmentLabel says so.

var (
	// DefaultAPIBaseURL is used when API_BASE_URL is absent.
	DefaultAPIBaseURL = "https://labriideas-api.onrender.com"

	// EnvironmentLabel is printed with the target so the operator always
	// knows which deployment the tool is about to modify.
	EnvironmentLabel = "Staging"
)
