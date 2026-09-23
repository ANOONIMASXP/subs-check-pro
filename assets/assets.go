package assets

import (
	_ "embed"
)

//go:embed backend.version
var EmbeddedSubStoreBackendVer []byte

//go:embed sub-store.min.js
var EmbeddedSubStore []byte

//go:embed geolite2.version
var EmbeddedGeoLite2Version []byte
