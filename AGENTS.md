# AGENTS.md — sapienza-timbre

Convenções para quem (humano ou agente) mexe neste repo. Complementa `CLAUDE.md`
(estrutura/comandos) e `SPEC.md` (arquitetura/regras).

## Estilo de código

- **pgx à mão, sem sqlc.** SQL cru em string literal, revisável. `ON CONFLICT` explícito.
- **Erros com `%w`.** Mensagens em português; identificadores e nomes de tabela em inglês.
- **Comentários em português**, explicando o *porquê* (não o óbvio). Densidade parecida
  com a Margot.
- **net/http puro** (`http.ServeMux` com padrões `METHOD /path`, `r.PathValue`). Sem
  chi/gin/echo.
- **Sem estado global mutável.** Dependências injetadas por construtor (`NewServer`, etc.).

## Banco e tenancy

- `public` = camada compartilhada do Timbre (control plane + identidade/audiência).
- `tenant_<id>` = operação por produtor; **sem coluna de produtor** (o schema isola).
- Todo acesso a dado de operação entra por `tenancy.WithTenant` **dentro de uma transação**.
- Tabelas de tenant referenciam produtor/comprador/carteira por **referência solta** (sem
  FK cross-schema), o que também facilita apagar a pedido.
- Só adicionar migrations forward. `public/NNNN_*.sql` (boot) e `tenant/NNNN_*.up.sql`
  (por produtor). Não reescrever schema — todas as fases já estão modeladas.

## Auth

- JWT nativo do Timbre (HS256, issuer `sapienza-timbre`), emitido e validado por
  `internal/auth`. Claims: `sub`=colaborador, `pid`=produtor, `perms[]`, `owner`, `sver`.
- Permissões granulares: `checkin | financeiro | relatorios | atendimento`. Owner passa
  sempre. Gate por `requirePermission`/`requireOwner`.

## Segurança

- Repo **público**: segredos nunca versionados. `gitleaks` no CI (`secret-scan.yml`).
- `TIMBRE_JWT_SECRET` é fail-closed no compose. `TIMBRE_ADMIN_TOKEN` vazio desliga o
  bootstrap de produtor.

## Testes

- Integração contra Postgres real via `TEST_DATABASE_URL` (pula com `t.Skip` sem ela).
- Compartilham um só Postgres → rodar com `go test -p 1 ./...`.
- Helper `internal/testutil.Pool` migra `public` e limpa o estado a cada aquisição.
- Toda peça delicada fecha com teste: a virada de lote (1.2), a concorrência de assento
  (1.3, N goroutines → exatamente um vencedor), o QR offline (1.5). Sem o teste, a etapa
  não fecha.

## Fora de escopo (sem decisão nova)

Apps nativos nas lojas, boleto, API pública/webhooks p/ terceiros, catracas, ERP/CRM,
NF-e, estandes, bebida/estacionamento, streaming/certificados, compra em grupo,
antecipação de recursos. O PWA de portaria cobre o caso de app nativo.
