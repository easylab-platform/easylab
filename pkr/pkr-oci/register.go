package oci

import "github.com/pkr/pkrkit"

func init() {
	pkrkit.Register("oci", NewHandler)
}
