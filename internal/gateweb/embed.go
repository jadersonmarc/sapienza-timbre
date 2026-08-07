// Package gateweb embute o PWA da portaria (assets estáticos) servido pelo binário em
// /gate. Operação offline: valida o QR com a chave pública (WebCrypto Ed25519), enfileira
// os check-ins localmente e sincroniza ao reconectar.
package gateweb

import (
	"embed"
	"io/fs"
)

//go:embed assets/*
var assets embed.FS

// FS devolve o filesystem enraizado em assets/ (para servir com http.FileServer).
func FS() fs.FS {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err)
	}
	return sub
}
