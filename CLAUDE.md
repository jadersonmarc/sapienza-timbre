# CLAUDE.md — sapienza-timbre

## O que é

Data plane do **Timbre**: bilheteria descentralizada da Sapienza Labs (ingressos como
NFT, assinatura Ed25519 verificável offline, carteira invisível por MPC). Repo novo,
**não** entra no `sapienza-core`. Reusa `../sapienza-kit` (tenancy). Ver `SPEC.md`
(arquitetura/regras) e `AGENTS.md` (convenções). Estado: **Etapa 1.1 (Fundação)**.

## Stack

- Go 1.26. Importa `github.com/jadersonmarc/sapienza-kit` (replace local `../sapienza-kit`),
  vendorizado (`go mod vendor`) para build hermético `-mod=vendor`.
- `pgx/v5` (à mão, **sem sqlc**). `golang-jwt/jwt/v5` (JWT nativo). `golang.org/x/crypto`
  (bcrypt). `google/uuid`.
- Interfaces trocáveis (padrão WhatsAppDriver da Margot): `ChainDriver`,
  `PaymentGateway`, `WalletProvider`, `Notifier` — default Noop/stub até suas etapas.

## Comandos

```bash
make build          # go build ./...
make vet            # go vet ./...
make test           # go test -p 1 ./...  (exige TEST_DATABASE_URL; pula sem ela)
make run            # go run ./cmd/server
make compose-up     # sobe Postgres próprio + binário
```

## Estrutura

- `cmd/server/main.go` — boot: config, pool, migra `public`, catch-up de tenants,
  mux `/health` + `/api/v1/`.
- `db/migrations/public/` — camada compartilhada (control plane + identidade/audiência).
- `db/migrations/tenant/` — operação por produtor (via `kit/tenancy.MigrationRunner`).
- `internal/config` — env → struct, valida obrigatórios.
- `internal/db` — pool + `MigratePublic` (runner do schema `public`).
- `internal/auth` — JWT nativo do Timbre (HS256) + bcrypt + permissões granulares.
- `internal/producer` — cria produtor e provisiona o schema tenant_<id>.
- `internal/store` — pgx à mão (control plane em `public`).
- `internal/catalog` — eventos/lotes/cupons (1.2) + setores/assentos/preços (1.3).
- `internal/inventory` — motor de reserva: Hold/Release/Confirm + varredura de expiração (1.3).
- `internal/api` — API do produto: guard/requirePermission/withTenant + handlers de catálogo/inventário.
- `internal/{chain,payment,wallet,notify}` — seams (interfaces + Noop/stub).

## Convenções (regras de ouro)

- **Banco próprio do Timbre.** `public` = camada compartilhada do Timbre (não é do
  core); `tenant_<id>` = operação por produtor. Um produtor = um schema.
- **`WithTenant` sempre em transação** — todo dado de operação por-produtor entra por ele.
- **Auth é nativa do Timbre** (o produtor é criado e autenticado aqui). O owner tem
  todas as permissões; as granulares são `checkin | financeiro | relatorios | atendimento`.
- **pgx à mão** (SQL cru, revisável). Erros com `%w`.
- **A rede nunca bloqueia a venda** — emissão em rede via fila/`ChainDriver` (Noop default).
- **Nenhum dado pessoal em payload de rede.** Vínculo pessoa↔carteira em `public.wallets`,
  apagável a pedido.
- **Preço/regra nunca chumbados.** Segredos nunca versionados; testes usam mocks.

## Restrições

- Não editar `../sapienza-kit`, `../sapienza-core`, `../sapienza-margot` fora do combinado.
- Não introduzir sqlc nem golang-migrate. Não criar microserviços — módulos são pacotes.
- O schema de todas as fases (1, 2 e 3) já está nas migrations da 1.1: **não reescrever
  schema depois**, só adicionar migrations forward.
