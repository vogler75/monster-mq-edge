package version

const (
	Version = "0.1.0-edge"
	Name    = "monstermq-edge"
)

func String() string {
	return Name + " " + Version
}
