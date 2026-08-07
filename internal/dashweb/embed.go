// Package dashweb embute o painel do produtor (assets estáticos) servido em /dash. O
// painel faz polling dos endpoints de agregação para acompanhar a venda em tempo real
// pelo celular.
package dashweb

import (
	"embed"
	"io/fs"
)

//go:embed assets/*
var assets embed.FS

// FS devolve o filesystem enraizado em assets/.
func FS() fs.FS {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err)
	}
	return sub
}
