// Package migrations embeds o SQL do Timbre.
//
//   - Public: a camada compartilhada do Timbre (control plane de produtores +
//     auth + identidade/audiência cross-produtor), aplicada uma vez no boot em
//     `public`. Como o Timbre roda no seu próprio banco, `public` é dele — não do
//     core.
//   - Tenant: tabelas por produtor, aplicadas a cada schema tenant_<id> via
//     kit/tenancy.MigrationRunner (que faz glob de *.up.sql na raiz do FS).
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed public/*.sql
var publicFS embed.FS

//go:embed tenant/*.up.sql
var tenantFS embed.FS

// Public é o FS enraizado nas migrations compartilhadas (public/).
var Public = mustSub(publicFS, "public")

// Tenant é o FS enraizado nas migrations de tenant (tenant/*.up.sql na raiz).
var Tenant = mustSub(tenantFS, "tenant")

func mustSub(f embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
