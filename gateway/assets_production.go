//go:build production

package main

import _ "embed"

//go:embed bootstrap_sa.json
var embeddedBootstrapCredential []byte
