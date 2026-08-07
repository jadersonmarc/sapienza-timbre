# SPEC — sapienza-timbre

## Tese

Bilheteria hoje é caixa registradora. O Timbre inverte as pontas: o público acumula um
histórico verificável do que viveu (e é dono dele); o produtor entra numa rede onde a
audiência circula entre eventos. Web3 aqui torna a presença comprovável sem depender da
palavra da plataforma. Cada ingresso tem seu próprio timbre — verificável por qualquer
um, falsificável por ninguém.

## Lugar no ecossistema Sapienza

Repo próprio (`sapienza-timbre`), fora do `sapienza-core`. O core é multitenancy de
assinante com cobrança recorrente; o Timbre tem **tenancy de produtor**, **receita
transacional** e um segundo público de altíssimo volume — o comprador, que sequer tem
conta. Misturar contaminaria os dois. Reusa o `sapienza-kit` para tenancy; segue o padrão
Control Plane / Data Plane da Margot. Mais um app no Coolify, no mesmo VPS.

## Topologia (decisão central)

- **Banco próprio.** O Timbre roda no seu próprio database. A Margot compartilha o
  Postgres do core porque é produto de assinatura do core; o Timbre tem tenancy de
  produtor nativa — se dividisse o DB do core, o `ListTenantSchemas`/`ApplyToAllTenants`
  do kit varreria `tenant_*` do core e da Margot. Banco próprio elimina a colisão.
- **`public` = camada compartilhada do Timbre.** Control plane (produtores + auth) e a
  camada cross-produtor (identidade/audiência/carteira/presença), inerentemente
  transversal — a trajetória de uma pessoa cruza vários produtores. `WithTenant` já faz
  `search_path = tenant_<id>, public`, então a operação de um produtor enxerga o
  compartilhado sem juggling.
- **`tenant_<id>` = operação por produtor.** Catálogo, venda, ingresso, rede, financeiro.
  Sem coluna de produtor — o schema é o isolamento.

## Decisões técnicas (fechadas)

Go + pgx/v5 (à mão, sem sqlc). PostgreSQL. Next.js no console/admin (etapas futuras).
Asaas com split no ato da venda. Ed25519 para assinatura do ingresso. Base + ERC-1155.
Carteira invisível por MPC, sem custódia de chave pela plataforma. Hold de assento no
Postgres (nunca Redis como fonte de verdade). Contrato em rede mínimo e imutável.

## Guardrails permanentes

1. A rede nunca bloqueia a venda. Concluído o pagamento, o ingresso existe e é válido;
   emissão em rede roda em fila. RPC fora não impede comprar nem entrar.
2. O QR é verificável offline (Ed25519, chave pública no app de portaria; sem banco/rede
   no portão).
3. Nenhum dado pessoal vai para a rede. Vínculo pessoa↔carteira em tabela comum, apagável.
4. Mapa de assentos é opcional por evento.
5. A Camada 3 (identidade/audiência) nunca é obrigatória.
6. Presença é intransferível por desenho (sem mecanismo de transferência).
7. Comercializa-se alcance consentido, jamais dado pessoal. Consentimento granular e
   revogável.
8. Sem promessa de retorno — pré-venda é sempre pré-compra de ingresso.

## Módulos (pacotes num binário, não microserviços)

`catalog, pricing, inventory, checkout, ticketing, gate, chain, ledger, presence,
audience, partners, console`. A operação é de uma pessoa.

## Interfaces centrais (padrão WhatsAppDriver da Margot)

`ChainDriver` (Mint/Transfer/Burn/Status), `WalletProvider` (EnsureWallet),
`PaymentGateway` (CreateCharge/HandleWebhook), `Notifier` (Send). Driver escolhido por
config, implementação trocável sem tocar no chamador. Default até suas etapas:
`NoopChainDriver`, `AsaasGateway` stub, `NoopWalletProvider`, `LogNotifier`.

## Invariantes garantidos por schema (não pela aplicação)

- Um assento não tem dois ingressos ativos — índice único parcial em `tickets(event_id,
  seat_id) WHERE status='active' AND seat_id IS NOT NULL`.
- Um assento não tem dois holds vivos — índice único parcial em `holds(event_id, seat_id)
  WHERE status='held'`.
- `transferable_after` nasce preenchido (imediato em Pix, 60 dias em cartão).
- `attendance_records` não tem coluna de transferência.
- Nenhuma tabela de payload de rede referencia dado pessoal.

**Resolvido na Etapa 1.3** (migration 0006): a exclusão hold×ticket (cross-table) virou um
único índice via `seat_occupancy (event_id, seat_id) WHERE NOT released`, onde tanto `Hold`
quanto a emissão de ingresso escrevem. `holds` passou a ser a reserva (grupo de N assentos).
Motor em `internal/inventory`: `Hold/Release/Confirm` + varredura de expiração (`FOR UPDATE
SKIP LOCKED`, TTL default 10 min, sweeper por tenant). Teste de concorrência (N goroutines →
1 vencedor) fecha a etapa.

## Roadmap (resumo)

- **Fase 1 — Operação e emissão:** 1.1 Fundação · 1.2 Catálogo · 1.3 Inventário/assentos
  · 1.4 Checkout · 1.5 Emissão · 1.6 Portaria · 1.7 Painéis · 1.8 Rede e financeiro.
- **Fase 2 — Propriedade e identidade:** transferência restrita/royalty, mercado
  secundário, passe de temporada, presença, panorama, descoberta, programa de produtores,
  divulgação.
- **Fase 3 — Audiência:** segmentação por presença, consentimento, recompensa, painel do
  patrocinador. **Precisa de leitura jurídica antes de construir.**

O schema de todas as fases já está nas migrations da 1.1 — não reescrever depois.
