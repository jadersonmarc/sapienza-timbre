# CLAUDE.md — sapienza-timbre

## O que é

Data plane do **Timbre**: bilheteria da Sapienza Labs. O ingresso é assinado em Ed25519 e
verificável OFFLINE na portaria (só com a chave pública). Repo novo, **não** entra no
`sapienza-core`. Reusa `../sapienza-kit` (tenancy). Ver `SPEC.md` (arquitetura/regras) e
`AGENTS.md` (convenções).

**O eixo on-chain é PROVA, não posse.** Isto é uma decisão tomada, não uma etapa pendente:

- **Não existe** MPC, carteira custodial, exportação ou importação de carteira externa,
  derivação hierárquica nem integração com Monitor. ERC-1155 fica dormente e
  `CHAIN_MINT_MODE=off` — nenhum caminho materializa token.
- **Existe** atestado de fechamento assinado em Ed25519 (chave SEPARADA da do QR), com
  registro canônico agregado e sem dado pessoal, `key_id` versionado (a verificação resolve
  a chave pelo key_id do atestado, nunca pela corrente) e verificação pública em `/a/[id]`.
- Âncora em cadeia em modo `log`: registra a intenção, `anchor_status` fica `none`, e nada
  é afirmado como registrado em cadeia sem transação real.
- Transferência, revenda, teto de revenda e royalty existem em **custódia de plataforma**,
  sem depender de cadeia nenhuma.

**Pagamento:** Asaas. A **bilheteria retém e repassa depois do evento** — a cobrança nasce
inteira na conta da plataforma e o repasse ao produtor vence alguns dias após a realização.
**Não existe split, nem subconta de produtor no gateway**: se o evento não acontece, o
dinheiro precisa estar com quem vai devolver. Sem Escrow, sem BaaS. **Só Pix e cartão —
boleto não existe no produto.** A taxa de plataforma é **10% flat para todo produtor**: o
programa de níveis foi extinto, e não existe rebate, tabela de níveis nem taxa efetiva por
produtor.

**A execução bancária do repasse NÃO existe**: nada transfere, saca ou valida titularidade de
conta. O produto calcula, registra e exibe a obrigação; marcar como pago é ação manual do
admin, com comprovante. Não há adiantamento antes do evento.


## Arquitetura (alinhamento pós-frontend)

Este repo é o **data plane / API JSON** do Timbre. **Toda a UI voltada a cliente vive no
frontend Next `../sapienza-timbre-web`** (comprador + **painel do produtor em `/painel`**),
que consome esta API por proxy server-side (cookie httpOnly + Bearer — o Go não precisa de
CORS de navegador).

- **Servido pelo Go:** `/api/v1/` (API), `/health`, e **`/gate`** (portaria PWA offline
  embutida — exceção intencional, crítica na porta).
- **`/dash` embutido (`internal/dashweb`): REMOVIDO.** O painel do produtor vive no `/painel`
  do Next; o painel administrativo da plataforma vive num `/admin` (também Next), com auth
  JWT própria (escopo "admin", papéis `admin`/`super_admin`).
- Domínios: API em `timbre-api.<dominio>`; site (comprador + painéis) em `timbre.<dominio>`.

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
- `internal/auth` — JWT nativo do Timbre (HS256) + bcrypt + permissões granulares; escopos
  separados: colaborador, comprador (audience "buyer") e admin de plataforma (audience "admin").
- `internal/producer` — cria produtor e provisiona o schema tenant_<id>.
- `internal/store` — pgx à mão (control plane em `public`).
- `internal/catalog` — eventos/lotes/cupons (1.2) + setores/assentos/preços (1.3).
- `internal/inventory` — motor de reserva: Hold/Release/Confirm + varredura de expiração (1.3).
- `internal/payment` — PaymentGateway: FakeGateway (default) e AsaasGateway (HTTP).
- `internal/checkout` — compra: sessão de checkout, webhook idempotente, razão, cortesias;
  e todo o ciclo de ESTORNO (política, pedido com quatro trilhas, execução total ou parcial,
  cancelamento de evento com devolução em massa).
- `internal/ticketing` — assinatura Ed25519 dos ingressos + verificador offline (só chave pública) (1.5).
- `internal/nft` — metadados públicos do ingresso (sem dado pessoal), estado, disputa, reemissão.
- `internal/attest` — fechamento do evento: registro canônico agregado, assinatura Ed25519,
  âncora (modo log), compromissos declarados e a cota de meia-entrada que vale na venda.
- `internal/gate` — portaria: valida QR offline, check-in + presença, reconciliação do sync (1.6/2.4).
- `internal/gateweb` — PWA da portaria (assets embutidos, servida em /gate) (1.6).
- `internal/dash` — agregações dos painéis (produtor + plataforma) (1.7).
- `internal/api` — API do produto: guard/withTenant + handlers de catálogo/inventário/checkout/portaria/painel/admin.
- `internal/chain` — seam de cadeia: interface + Noop/Base e a fila de âncora. Dormente.
- `internal/payout` — obrigação de repasse por evento: cálculo, vencimento, retenção com motivo e marcação manual de pago. Não executa transferência.
- `internal/pricing` — preço: face + conveniência, com a taxa de plataforma de 10% flat.
- `internal/fees` — tabela de tarifas do gateway (nenhum valor de tarifa é chumbado).
- `internal/transfer` — transferência restrita: teto de revenda + royalty + reatribuição de dono (2.1).
- `internal/market` — mercado secundário: anúncio, compra pública, procedência, receita (2.2).
- `internal/season` — passe de temporada: emite um ingresso por data, destacável/repassável (2.3).
- `internal/panorama` — panorama de passeios: mapa/linha do tempo + retrospectiva anual (2.5).
- `internal/trust` — descoberta e confiança: reviews (só quem entrou), reputação, descoberta (2.6).
- `internal/promo` — divulgação: campanhas UTM/pixels, lista de espera + aviso na virada, perfil do público (2.8).
- `internal/audience` — Fase 3: segmentação por presença, consentimento granular, recompensa, alcance sem identidade.
- `internal/{chain,payment,notify}` — seams (interfaces + Noop/stub).

## Convenções (regras de ouro)

- **Banco próprio do Timbre.** `public` = camada compartilhada do Timbre (não é do
  core); `tenant_<id>` = operação por produtor. Um produtor = um schema.
- **`WithTenant` sempre em transação** — todo dado de operação por-produtor entra por ele.
- **Auth é nativa do Timbre** (o produtor é criado e autenticado aqui). O owner tem
  todas as permissões; as granulares são `checkin | financeiro | relatorios | atendimento`.
- **pgx à mão** (SQL cru, revisável). Erros com `%w`.
- **A rede nunca bloqueia a venda** — a âncora é assíncrona e o `ChainDriver` é Noop por
  default. Derrubar o RPC não impede venda nem entrada na portaria.
- **Nenhum dado pessoal no registro canônico nem nos metadados públicos** — só agregados.
- **Preço é um só:** 10% flat, num ponto de configuração. Qualquer caminho de venda (compra
  comum, passe de temporada, mercado secundário) apura igual.
- **Preço/regra nunca chumbados.** Segredos nunca versionados; testes usam mocks.

## Restrições

- Não editar `../sapienza-kit`, `../sapienza-core`, `../sapienza-margot` fora do combinado.
- Não introduzir sqlc nem golang-migrate. Não criar microserviços — módulos são pacotes.
- Só adicionar migrations **forward**; não reescrever migration já aplicada.
- **Meia-entrada: 40% é DEFAULT, não trava.** O produtor pode definir menos; o sistema avisa e
  registra a escolha na trilha. A meia sempre consome o estoque do tipo pai — estoque próprio
  que soma continua fora.
- **Não reintroduzir** MPC, carteira, mint, Escrow, BaaS, boleto, programa de níveis, **split
  por compra nem subconta de produtor no gateway**. São caminhos descartados por decisão, não
  pendências — e `internal/payout/legacy_test.go` varre o código para garantir que não voltem.
- **Não implementar** transferência bancária, saque ou validação de titularidade de conta, nem
  abater crédito a recuperar de repasse futuro automaticamente.
