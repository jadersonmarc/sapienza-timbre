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

## Split de pagamento (repasse ao produtor)

O comprador paga **face + conveniência**. O produtor recebe o **face limpo**, dividido na
própria cobrança pelo gateway. A margem do Timbre é a conveniência menos a tarifa do
gateway — 10% do face, para todo produtor.

O preço sai de uma fórmula fechada, porque a tarifa percentual do gateway incide sobre o
valor cobrado, que já contém a conveniência:

    V = (F × (1 + p) + b) / (1 − a)

`F` face · `p` taxa de plataforma (10%) · `a` e `b` a tarifa do gateway para o método e a
faixa de parcelamento. `V` arredonda **para cima**: um centavo a menos deixaria o líquido
abaixo do face e o gateway recusaria o split. As tarifas vêm de `GET /v3/myAccount/fees`,
com cache e a última tabela persistida como contingência — nenhum valor de tarifa é
chumbado no código.

### Conta do produtor

A conta de recebimento (subconta padrão — **não é BaaS, não é conta escrow**) pertence ao
produtor e é reusada em todos os eventos dele. Estados:

    sem_conta → criada_aguardando_docs → em_analise → aprovada | reprovada

Evento pode ser criado, editado e configurado em qualquer estado. **Só abrir venda exige
`aprovada`** — o produtor monta o evento enquanto a análise corre. A `apiKey` da subconta é
descartada na criação: o split não precisa dela e não há coluna para guardá-la.

### Antes da primeira subconta em PRODUÇÃO

A criação de subcontas via API abre um **período de avaliação regulatória**: no máximo
**10 subcontas de titulares distintos** e **60 dias corridos** contados da primeira. Ao
estourar, criação e emissão são bloqueadas.

> **Não crie a primeira subconta em produção antes de a documentação da avaliação
> regulatória estar pronta para envio.** O relógio começa na primeira criação, não quando
> você estiver pronto. Em Sandbox o limite é de 20 por dia e não atrapalha os testes.

O código alerta ao chegar em 7 subcontas e recusa a criação no teto.

### Confirmação anual de dados comerciais

Exigência regulatória: sem confirmar, a subconta perde o uso da API. A data vem do gateway
(`commercialInfoExpiration`) e é atualizada por **webhook**
(`ACCOUNT_STATUS_COMMERCIAL_INFO_EXPIRING_SOON`) — não por polling, porque o campo só muda
uma vez por ano e consulta proativa gasta rate limit à toa.

### Divergência na liquidação

A cobrança é criada semanas antes de ser paga. Se a tabela de tarifas mudar nesse intervalo,
um valor de split que passou na criação pode divergir na liquidação: o gateway bloqueia o
valor e dá **2 dias úteis** para ajustar. É cenário esperado, não defeito — vira alerta
operacional, e o repasse fica `BLOCKED`. Passado o prazo, `CANCELLED` e resolução manual.

### Antecipação de recebíveis

Se a conta do Timbre passar a usar antecipação de recebíveis, o split é recusado com
`RECEIVABLE_UNIT_AFFECTED_BY_EXTERNAL_CONTRACTUAL_EFFECT`. O motivo da recusa fica gravado
em `split_transfers.refusal_reason`.

### Line-up

O rateio entre artistas é **informativo**: alimenta o painel do produtor e não movimenta
dinheiro. Artista não é recebedor no gateway — quem paga o artista é o produtor.

## Estorno

O comprador recebe **face + conveniência de volta**: o produtor devolve o face, a plataforma
devolve a conveniência. Ninguém lucra com cancelamento. A tarifa que o gateway reteve não
volta e é custo da plataforma — registrada em `refunds.gateway_fee_cents` para o custo real
aparecer, nunca escondida no valor do produtor.

O razão recebe **três** lançamentos, espelhando as três linhas da venda:

| kind | valor | quem devolve |
|---|---|---|
| `estorno` | `-face` | produtor |
| `estorno_taxa` | `-conveniência` | plataforma |
| `retencao` | `-proporcional` | desfaz a reserva de contestação da parte estornada |

A terceira é a que passa despercebida: `NetDue` **subtrai** a retenção de 5% enquanto ela
está retida. Sem desfazê-la, uma venda estornada dentro dos 60 dias deixaria o produtor com
saldo negativo por uma venda que não existe mais.

### Duas fases, e por quê

A chamada ao gateway acontece **fora da transação**, ao contrário da criação da cobrança. A
razão é assimétrica: uma cobrança órfã (criada e revertida) é inofensiva — ninguém pagou.
Um estorno órfão é dinheiro que voltou ao comprador com o ingresso ainda válido e sem
registro nenhum.

    1. transação curta  → grava a intenção em `refunds` e reserva os ingressos, e COMMITA
    2. fora de transação → estorna no gateway
    3. transação curta  → queima ingressos, devolve capacidade, lança o razão

Falhar na fase 2 deixa `refunds.status='failed'` com o motivo, os ingressos soltos e nada
aplicado pela metade. A idempotência é do índice único parcial em `refund_tickets`: um
ingresso não entra em dois estornos vivos, e é o schema que garante isso.

### Estorno parcial

Estornar 1 de 4 devolve capacidade só daquele lote, libera só aquele assento e queima só
aquele ingresso. O pedido fica `partially_refunded`, e os outros três continuam entrando na
portaria. O valor por ingresso sai do preço do item que ele ocupa, ajustado para fechar
**exatamente** o face do pedido — o cupom desconta o pedido, não o item, e dividir o total
deixaria centavo sobrando.

### Reversão do repasse

De onde o dinheiro sai decide quem fica devendo (registrado em `split_refunds.source`):

| Situação | Origem | Efeito |
|---|---|---|
| Split ainda não liquidado | `not_settled` | o estorno da cobrança cancela o split junto |
| Venda centralizada | `platform_balance` | o dinheiro nunca saiu da plataforma |
| Split liquidado, subconta cobre | `producer` | o gateway puxa da subconta do produtor |
| Split liquidado, sem saldo | `platform_covered` | a plataforma cobre e o produtor fica devendo |

**O comprador é estornado mesmo quando a subconta não cobre.** Deixar alguém sem o dinheiro
porque o produtor já sacou não é opção. A dívida vira `NetDue` negativo e sai dos próximos
repasses sozinho — `SettleDue` não materializa payout com líquido negativo — e aparece no
painel do produtor com a lista de estornos que a compõem. Acima de `RefundDebtAlertCents`
(provisório: R$ 500) o produtor aparece como alerta no painel da plataforma.

`split_transfers` tem uma linha por PEDIDO, então o parcial nunca a sobrescreve: a reversão
é registrada por ingresso em `split_refunds`, e `split_status` só vai a `REFUNDED` quando não
sobra face no pedido.

### Guardas

- **Entrada registrada**: ingresso que já passou na portaria não é estornável pelo produtor.
  Só admin, com motivo obrigatório — quem usou o ingresso consumiu o serviço.
- **Evento fechado**: o estorno é permitido e **republica o atestado**, em versão nova com
  `supersedes_id`. A anterior continua acessível. Sem isso, a comprovação de público mente.
- **Eco do webhook**: o aviso do gateway sobre o estorno que nós mesmos originamos não pode
  ser lido como devolução externa — senão queimaria o que sobrou do pedido. A janela de eco
  é de 10 minutos (provisório: o aviso não traz o id do estorno de forma confiável).

### Quem pode estornar

    POST /api/v1/orders/{id}/refund                              # owner do produtor
    POST /api/v1/admin/producers/{producerId}/orders/{id}/refund  # admin, motivo obrigatório

Corpo vazio estorna o pedido inteiro; `{"ticket_ids": [...]}` estorna os escolhidos. A fila
de pedidos do comprador, com política e aprovação, vem depois.

## Testes

```bash
make test   # go test -p 1 ./...  (exige TEST_DATABASE_URL; pula sem ela)
```

## Deploy

Coolify no VPS Sapienza, a partir do `Dockerfile` (build hermético `-mod=vendor`,
runtime distroless). Aponte `DATABASE_URL` para um database dedicado do Timbre. Ver
`SPEC.md` (arquitetura), `CLAUDE.md` (estrutura) e `AGENTS.md` (convenções).
