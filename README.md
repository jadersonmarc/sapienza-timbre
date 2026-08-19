# sapienza-timbre

Bilheteria descentralizada da **Sapienza Labs**. Organizadores e participantes gerenciam
ingressos de evento, com contratos inteligentes e ingressos emitidos como NFT. Cada
ingresso tem seu próprio *timbre* — verificável por qualquer um, falsificável por ninguém.

Backend em Go, data plane no espírito Control/Data da suíte Sapienza. Reusa o
`sapienza-kit` para tenancy. Roda no seu **próprio** Postgres.

> Estado: **Etapa 1.1 (Fundação)**. O binário sobe, migra e responde health check; um
> produtor é criado e autenticado com permissões granulares por colaborador. As migrations
> já contemplam todo o modelo de dados (fases 1–3). Catálogo, checkout, emissão, portaria,
> painéis e rede vêm nas etapas seguintes.

## Rodar local

```bash
cp .env.example .env      # defina TIMBRE_JWT_SECRET e TIMBRE_ADMIN_TOKEN
export TIMBRE_JWT_SECRET=$(openssl rand -base64 48)
export TIMBRE_ADMIN_TOKEN=$(openssl rand -hex 16)
docker compose up --build # Postgres próprio + binário na :8082
```

Ou sem Docker, apontando para um Postgres seu:

```bash
export DATABASE_URL=postgres://postgres:postgres@localhost:5432/timbre
export TIMBRE_JWT_SECRET=... TIMBRE_ADMIN_TOKEN=...
make run
```

## Provar o fluxo (Etapa 1.1)

```bash
curl -s localhost:8082/health          # -> ok

# criar produtor (bootstrap por admin token)
curl -sX POST localhost:8082/api/v1/producers \
  -H "X-Admin-Token: $TIMBRE_ADMIN_TOKEN" \
  -d '{"name":"Casa X","owner_email":"owner@x.com","owner_password":"senha1234"}'

# logar e provar a sessão
TOKEN=$(curl -sX POST localhost:8082/api/v1/auth/login \
  -d '{"email":"owner@x.com","password":"senha1234"}' | jq -r .token)
curl -s localhost:8082/api/v1/me -H "Authorization: Bearer $TOKEN"

# convidar colaborador com permissões granulares (owner)
curl -sX POST localhost:8082/api/v1/collaborators -H "Authorization: Bearer $TOKEN" \
  -d '{"email":"caixa@x.com","password":"senha1234","permissions":["checkin","financeiro"]}'
```

## Config

| Variável | Obrigatória | Descrição |
|---|---|---|
| `DATABASE_URL` | sim | Postgres **próprio** do Timbre |
| `TIMBRE_JWT_SECRET` | sim | assina/valida a sessão nativa (HS256) |
| `TIMBRE_ADMIN_TOKEN` | não | bootstrap de produtor; vazio desliga `POST /producers` |
| `TIMBRE_ADMIN_EMAIL` | não | e-mail do primeiro `super_admin` do `/admin` (seed no boot) |
| `TIMBRE_ADMIN_PASSWORD` | não | senha do primeiro `super_admin` (obrigatória se `TIMBRE_ADMIN_EMAIL` for definida) |
| `PORT` | não | default `8082` |
| `LOG_LEVEL` | não | `debug\|info\|warn\|error` (default `info`) |
| `TIMBRE_ENC_KEY` | não | reservado (AES por-tenant), não usado na 1.1 |
| `ASAAS_API_KEY` | não | gateway (Etapa 1.4); vazio = stub |

## Testes

```bash
make test   # go test -p 1 ./...  (exige TEST_DATABASE_URL; pula sem ela)
```

## Deploy

Coolify no VPS Sapienza, a partir do `Dockerfile` (build hermético `-mod=vendor`,
runtime distroless). Aponte `DATABASE_URL` para um database dedicado do Timbre. Ver
`SPEC.md` (arquitetura), `CLAUDE.md` (estrutura) e `AGENTS.md` (convenções).
