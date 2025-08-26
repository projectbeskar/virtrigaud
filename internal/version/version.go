package version

var (
	// Version is the version of the virtrigaud manager (overridden via -ldflags)
	Version = "dev"
	// GitSHA is the git commit SHA (overridden via -ldflags)
	GitSHA = "unknown"
)

// String returns a formatted version string
func String() string {
	return Version + " (" + GitSHA + ")"
}
