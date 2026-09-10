package substore

import (
	_ "embed"
)

//go:embed init.js
var initScriptBytes []byte
