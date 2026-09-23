// Package version guarda la versión de la suite. build.ps1 la pisa con -ldflags
// (-X) para estampar el commit exacto en cada exe.
package version

// Version sigue semver; Commit es el hash corto del árbol que se compiló.
var (
	Version = "1.0.0"
	Commit  = "dev"
)

// String devuelve "1.0.0" o "1.0.0+abc1234" cuando hay commit estampado.
func String() string {
	if Commit == "" || Commit == "dev" {
		return Version
	}
	return Version + "+" + Commit
}
