package version

// Version is the build version. It can be overridden at build time via:
//   go build -ldflags "-X monstermq.io/edge/internal/version.Version=1.2.3+abcdef"
var Version = "0.1.0-edge"

const Name = "monstermq-edge"

func String() string {
	return Name + " " + Version
}
