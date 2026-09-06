// Package adapters blank-imports third-party protocol adapter packages so
// their Register() calls (via init) are pulled into the binary. To enable a
// protocol, add an import here and rebuild.
package adapters

import (
	_ "github.com/pkr/pkr-cargo"
	_ "github.com/pkr/pkr-composer"
	_ "github.com/pkr/pkr-conan"
	_ "github.com/pkr/pkr-generic"
	_ "github.com/pkr/pkr-go"
	_ "github.com/pkr/pkr-helm"
	_ "github.com/pkr/pkr-hex"
	_ "github.com/pkr/pkr-maven"
	_ "github.com/pkr/pkr-npm"
	_ "github.com/pkr/pkr-nuget"
	_ "github.com/pkr/pkr-oci"
	_ "github.com/pkr/pkr-system"
	_ "github.com/pkr/pkr-pub"
	_ "github.com/pkr/pkr-pypi"
	_ "github.com/pkr/pkr-rubygems"
	_ "github.com/pkr/pkr-swift"
)
