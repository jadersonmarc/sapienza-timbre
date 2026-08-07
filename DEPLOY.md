# DEPLOY — sapienza-timbre (Coolify / VPS Hostinger)

Deploy do binário Go a partir do `Dockerfile` (build hermético `-mod=vendor`, runtime
distroless). O Timbre sobe **independente** dos outros apps e roda no seu **próprio
database** — não compartilha o Postgres do core (ver `SPEC.md`).

O app **migra sozinho no boot** (schema `public` + catch-up dos schemas de produtor,
idempotente). Não há passo de migration separado no pipeline.

## Pré-requisitos

- Coolify instalado na VPS (Hostinger), com um projeto/servidor onde criar recursos.
- Repositório no GitHub (público) conectado ao Coolify (GitHub App ou deploy key).

## Passo 1 — Banco de dados (dedicado ao Timbre)

No Coolify: **New Resource → Database → PostgreSQL 16**.

- Nome sugerido: `timbre-postgres`; database `timbre`.
- Após criar, copie a **Internal Connection URL** (rede interna do Coolify, ex.:
  `postgres://<user>:<pass>@<service>:5432/timbre`). Ela vira o `DATABASE_URL` do app.
- **Não** use o Postgres do core: o Timbre cria schemas `tenant_<id>` próprios e um
  database separado evita colisão de namespace.

## Passo 2 — Aplicação

**New Resource → Application → From GitHub → (este repo)**.

- **Build Pack: Dockerfile** (Coolify detecta o `Dockerfile` na raiz).
- **Port (Ports Exposes): `8082`** (o `EXPOSE`/`PORT` do app).
- **Instâncias: 1.** As migrations rodam no boot; mantenha 1 réplica até haver um passo
  de migration desacoplado.

## Passo 3 — Variáveis de ambiente

Em **Environment Variables** do app:

| Variável | Valor |
|---|---|
| `DATABASE_URL` | Internal Connection URL do Passo 1 |
| `TIMBRE_JWT_SECRET` | `openssl rand -base64 48` — **guarde**, é a chave da sessão |
| `TIMBRE_ADMIN_TOKEN` | `openssl rand -hex 16` — bootstrap de produtor |
| `PORT` | `8082` |
| `LOG_LEVEL` | `info` |
| `ASAAS_API_KEY` | (opcional; vazio = stub até a Etapa 1.4) |

Segredos ficam **só no Coolify**, nunca no repo (o CI roda gitleaks). Marque-os como
secretos no Coolify quando possível.

## Passo 4 — Healthcheck

O runtime é distroless (sem shell/curl), então **não** há `HEALTHCHECK` no Dockerfile —
configure no Coolify: **Health Check → HTTP GET `/health` na porta `8082`** (200 = ok; o
endpoint faz ping no banco). Isso dá o gate de readiness em cada deploy.

## Passo 5 — Domínio e TLS

Em **Domains**, aponte um subdomínio (ex.: `timbre.suaempresa.com`) para a porta `8082`.
O Coolify emite o TLS (Let's Encrypt). O app escuta em `0.0.0.0:8082`.

## Passo 6 — Deploy e verificação

Dispare **Deploy**. Acompanhe o log: deve mostrar `timbre up` e as migrations aplicadas.

```bash
curl -s https://timbre.suaempresa.com/health          # -> ok

# criar o primeiro produtor (bootstrap)
curl -sX POST https://timbre.suaempresa.com/api/v1/producers \
  -H "X-Admin-Token: $TIMBRE_ADMIN_TOKEN" \
  -d '{"name":"Casa X","owner_email":"owner@x.com","owner_password":"<senha forte>"}'
```

Push na branch de produção → Coolify re-builda e faz deploy (auto-deploy on push, se
habilitado). O boot re-aplica migrations idempotentes.

## Operação

- **Rotacionar `TIMBRE_JWT_SECRET`** invalida todas as sessões (todo mundo reloga) — é o
  botão de pânico. **Rotacionar `TIMBRE_ADMIN_TOKEN`** só afeta a criação de produtor.
- **Backup**: use o backup do PostgreSQL do Coolify no `timbre-postgres`.
- **Rollback**: redeploy da imagem/commit anterior pelo Coolify; migrations são
  forward-only (não há down) — evite reverter para um commit com schema mais novo já
  aplicado.
