# sapienza-timbre

Bilheteria da **Sapienza Labs**. Cada ingresso tem seu próprio *timbre*: uma assinatura
Ed25519 que a portaria verifica **offline**, só com a chave pública — verificável por
qualquer um, falsificável por ninguém.

Backend em Go, data plane no espírito Control/Data da suíte Sapienza. Reusa o
`sapienza-kit` para tenancy. Roda no seu **próprio** Postgres.

> **O que este produto é.** Venda (Pix e cartão), emissão
> assinada, portaria offline, estorno em todas as suas formas, e **prova de público**: o
> fechamento do evento gera um registro canônico agregado, sem dado pessoal, assinado e
> verificável publicamente.
>
> **O que ele não é.** Não há carteira, MPC, mint de token, exportação para carteira
> externa, Escrow, BaaS nem boleto. O eixo on-chain é prova por âncora, não posse por
> token, e a âncora está em modo `log` — nada é afirmado como registrado em cadeia sem
> transação real. Transferência, revenda, teto e royalty funcionam em custódia de
> plataforma, sem depender de cadeia.

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

## Provar o fluxo

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

## Preço e repasse ao produtor

O comprador paga **face + conveniência**. O produtor recebe o **face limpo**. A margem do
Timbre é a conveniência menos a tarifa do gateway — 10% do face, para todo produtor.

O preço sai de uma fórmula fechada, porque a tarifa percentual do gateway incide sobre o
valor cobrado, que já contém a conveniência:

    V = (F × (1 + p) + b) / (1 − a)

`F` face · `p` taxa de plataforma (10%) · `a` e `b` a tarifa do gateway para o método e a
faixa de parcelamento. As tarifas vêm de `GET /v3/myAccount/fees`, com cache e a última
tabela persistida como contingência — nenhum valor de tarifa é chumbado no código.

### A bilheteria retém e repassa depois do evento

**Não há split.** A cobrança nasce inteira na conta da plataforma, e o repasse ao produtor
acontece **depois da realização do evento** — o mesmo modelo de Sympla, Ingresso.com e
Eventim. O motivo é um só: se o evento não acontece, o dinheiro precisa estar com quem vai
devolver.

O produtor **não tem conta no gateway**. O único cadastro que ele faz é a chave Pix de
recebimento, e é ela que a publicação do evento exige — abrir venda sem destino para o
dinheiro deixa o repasse sem para onde ir, em silêncio, na hora de pagar.

### A obrigação de repasse

`event_payouts` tem uma linha por evento. O razão continua sendo contabilidade (o que
aconteceu); isto é **obrigação**: quanto, a quem e até quando.

| campo | o que é |
|---|---|
| `gross_face_cents` | soma das faces vendidas |
| `refunded_face_cents` | faces estornadas |
| `platform_fee_cents` | a conveniência — é da plataforma, não entra no líquido |
| `gateway_fee_cents` | tarifa retida nas devoluções, conforme `refund_gateway_fee_bearer` |
| `net_due_cents` | o que o produtor recebe |
| `due_at` | vencimento, contado da realização |

    accruing → pending → paid
       ↘ on_hold ↗        ↘ cancelled

Enquanto o evento não acontece a linha fica `accruing` e cada venda ou estorno a atualiza.
A **realização** não depende de ninguém clicar em nada: `status='finished'` antecipa, e
`COALESCE(ends_at, starts_at)` no passado resolve o resto. Aí ela vira `pending`, com
`due_at = realização + payout_delay_days`.

`payout_delay_days` é parâmetro **por produtor com sobrescrita por evento**
(`payout_settings`, mesmo desenho da política de devolução), default 7 no banco. Mudar o
prazo reescreve os vencimentos já gravados — deixar as datas antigas de pé mostraria ao
produtor uma promessa que ninguém mais tem.

Motivos de `on_hold` são **lista fechada**, com ator e data registrados: `evento_cancelado`,
`disputa_aberta`, `verificacao_bancaria`, `decisao_admin`. Reter dinheiro de alguém por um
motivo que não está em lugar nenhum é decisão que ninguém consegue revisar depois — e o
produtor lê o texto correspondente no painel dele. A retenção por falta de chave Pix o
próprio sistema põe e tira.

### A execução bancária NÃO existe

Não há transferência, saque, validação de titularidade de conta nem antifraude de dados
bancários. O que existe é **cálculo, registro e exibição**. Marcar como pago é ação manual do
admin, com a referência da transferência — sem ela, "pago" vira palavra contra palavra na
primeira divergência.

Também não há adiantamento antes do evento: ele reintroduziria exatamente o risco que a
retenção elimina.

### Line-up

O rateio entre artistas é **informativo**: alimenta o painel do produtor e não movimenta
dinheiro. Artista não é recebedor no gateway — quem paga o artista é o produtor.

## Estorno

O comprador recebe **face + conveniência de volta**: o produtor devolve o face, a plataforma
devolve a conveniência. Ninguém lucra com cancelamento. A tarifa que o gateway reteve não
volta e é custo da plataforma — registrada em `refunds.gateway_fee_cents` para o custo real
aparecer, nunca escondida no valor do produtor.

O razão recebe **dois** lançamentos, espelhando as duas linhas da venda:

| kind | valor | quem devolve |
|---|---|---|
| `estorno` | `-face` | sai do resultado do produtor |
| `estorno_taxa` | `-conveniência` | sai da receita da plataforma |

Separados porque quem devolve cada parte é diferente — juntá-los foi o erro que descontava
do produtor os 10% que nunca foram dele.

Não há mais reserva de contestação de 5%/60d sobre o repasse do produtor: com a bilheteria
retendo o valor até depois do evento, a reserva é da plataforma por construção.

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

### O que o estorno faz com o repasse

Devolve dinheiro que **está na conta da plataforma**. Abate `refunded_face_cents` do repasse
do evento e pronto: não puxa valor de conta de terceiro, não gera saldo devedor e não abate
de repasse futuro. O cenário "o produtor já sacou" deixou de existir — ele não saca antes do
evento.

**Exceção, e só ela:** estorno depois de o repasse já ter sido **liquidado**. O dinheiro saiu
e o comprador precisa ser devolvido assim mesmo, então vira **crédito a recuperar**
(`recoverable_credits`), registrado e visível para o produtor e para o admin. Nada é abatido
automaticamente: não há repasse futuro garantido, e um desconto silencioso no evento seguinte
é o tipo de número que ninguém aceita.

Cancelamento de evento é onde o modelo mais ajuda: o dinheiro está com a plataforma, o
estorno em massa não depende de recuperar nada de ninguém, e o repasse do evento vai para
`cancelled`.

### Guardas

- **Entrada registrada**: ingresso que já passou na portaria não é estornável pelo produtor.
  Só admin, com motivo obrigatório — quem usou o ingresso consumiu o serviço.
- **Evento fechado**: o estorno é permitido e **republica o atestado**, em versão nova com
  `supersedes_id`. A anterior continua acessível. Sem isso, a comprovação de público mente.
- **Eco do webhook**: o aviso do gateway sobre o estorno que nós mesmos originamos não pode
  ser lido como devolução externa — senão queimaria o que sobrou do pedido. A janela de eco
  é de 10 minutos (provisório: o aviso não traz o id do estorno de forma confiável).

### Política: o que a casa promete

Por evento, com o default do produtor como herança e o embutido como último recurso.
Nenhum valor é chumbado — o default só vale enquanto ninguém configurar.

| Campo | Default | O que decide |
|---|---|---|
| `withdrawal_window_days` | 7 | janela de arrependimento, contada da COMPRA |
| `withdrawal_min_hours_before_event` | 0 | antecedência mínima do evento |
| `refund_gateway_fee_bearer` | `platform` | quem absorve a tarifa retida pelo gateway |
| `producer_discretionary_enabled` | `true` | se a casa analisa pedido fora da janela |
| `discretionary_response_hours` | 72 | prazo de resposta (vencido = atrasado, nunca aprovado) |
| `checkin_blocks_refund` | `true` | entrada registrada bloqueia |

Os **7 dias são piso**, não default editável: é o direito de arrependimento do art. 49 do
CDC. O produtor oferece mais, nunca menos, e quem garante isso é o `CHECK` da tabela.

### As quatro trilhas

A trilha é **derivada da política** no momento do pedido, nunca escolhida por quem pede —
senão o comprador se autoconcederia o caminho automático.

| Trilha | Quem | Autorização |
|---|---|---|
| `withdrawal` | comprador, dentro da janela | automática: é direito, não passa pela casa |
| `discretionary` | comprador, fora da janela | fila do produtor; recusa exige motivo |
| `producer_initiated` | a casa cancelando | sem fila — quem pede já é quem decide |
| `admin_override` | a plataforma | passa por cima das guardas, motivo obrigatório |

**Silêncio não aprova.** O prazo vencido marca o pedido como atrasado na fila e nada mais:
aprovação automática moveria dinheiro sem ninguém decidir.

Uma tentativa barrada (entrada registrada, por exemplo) **sai do estado vivo**. O índice
único guarda um pedido vivo por compra, e um pedido travado ali trancaria a ordem para todo
mundo — inclusive para o admin que vem justamente para passar por cima da guarda que barrou.

### Auditoria

Toda transição entra em `refund_request_events`: data, ator, papel, de-para e motivo.
Append-only — decisão tomada não se edita, e uma decisão que muda vira outra linha. É o que
o produtor mostra quando o comprador reclama, e o que a plataforma mostra quando o produtor
reclama.

### Cancelamento de evento

`POST /events/{id}/cancel` cancela **e devolve o dinheiro de todo mundo**. Antes era só uma
transição de status: o evento sumia do diretório e cada comprador ficava com ingresso válido
e dinheiro pago, descobrindo sozinho — o produtor achava que tinha resolvido.

A devolução não acontece na requisição. Um evento de mil ingressos seriam mil chamadas ao
gateway com o produtor olhando para uma tela travada, e um timeout no meio deixaria metade
devolvida sem registro de onde parou. A rota enfileira uma devolução por pedido pago
(`refund_jobs`, uma por pedido — cancelar duas vezes não vira duas devoluções) e **avisa
todos os compradores na hora**: quem tinha ingresso para amanhã precisa saber hoje, mesmo
que o dinheiro leve dias para voltar.

O worker segue a forma do `AnchorWorker` — tentativas, backoff exponencial com teto, motivo
persistido — com uma diferença deliberada: lá a chamada externa acontece dentro da
transação, aqui não pode. Esgotadas as tentativas, a devolução vira **trabalho manual** na
fila do `/admin`, com o motivo à vista e um botão para reenfileirar. Dinheiro que não voltou
precisa de alguém, não de silêncio.

Cancelamento é `admin_override` em termos de autorização, ainda que disparado pelo produtor:
ingresso que já entrou também é devolvido — o evento não aconteceu.

Se o evento já estava fechado, o registro canônico é republicado **uma vez, ao fim do lote**.
Enquanto o lote corre, o estorno avulso não republica: uma versão por pedido devolvido faria
o atestado virar ruído em vez de prova.

    GET /api/v1/events/{id}/cancellation                          # total, concluídos, falhos
    GET /api/v1/admin/refund-jobs/failed                          # fila de resolução manual
    POST /api/v1/admin/producers/{pid}/refund-jobs/{id}/retry

### Rotas

    GET|PUT /api/v1/refund-policy                                  # default do produtor
    GET|PUT /api/v1/events/{id}/refund-policy                      # do evento
    GET     /api/v1/public/events/{id}/refund-policy               # o que o comprador lê

    POST /api/v1/public/me/orders/{id}/refund-request              # comprador
    GET  /api/v1/public/me/refund-requests

    GET  /api/v1/refund-requests?status=pending                    # fila do produtor
    GET  /api/v1/refund-requests/{id}/history                      # trilha de auditoria
    POST /api/v1/refund-requests/{id}/approve|reject

    POST /api/v1/orders/{id}/refund                                # a casa cancelando
    POST /api/v1/admin/producers/{pid}/orders/{id}/refund          # admin
    POST /api/v1/admin/producers/{pid}/refund-requests/{id}/approve|reject

Corpo vazio estorna o pedido inteiro; `{"ticket_ids": [...]}` estorna os escolhidos.

## Meia-entrada e combo

### Cota de meia (Lei 12.933/2013)

A cota de **40% dos ingressos disponíveis** vale mesmo sem o produtor declarar nada: a
obrigação é da lei, não da declaração. O compromisso `meia_entrada_cota`, que até aqui só
era reportado no atestado, passa a **valer na venda** — uma cota que não barra nada é uma
promessa sem consequência. Declarar acima de 40% é direito do produtor; abaixo é recusado.

Esgotada a cota, a meia sai de venda e a **inteira continua**: acabou a meia, não o evento.
A checagem acontece em dois pontos, de propósito: na seleção (para a pessoa não preencher a
ficha inteira e ouvir não no fim) e na criação da ordem (onde a ficha nominal pode ter
mudado quem é meia). A base é a soma das quantidades dos lotes, e o concedido é contado em
**ingressos emitidos** — é assim que a lei mede, e é o que faz um combo de duas meias
consumir dois da cota.

`GET /public/events/{id}` publica `half_price` com cota, concedido e restante. Não é
enfeite: o art. 1º, §1º obriga a informar a disponibilidade de meia em todos os pontos de
venda.

### Combo (duplo, trio, grupo)

Combo é **quantidade mínima de compra**, não ingresso especial. Um "ingresso duplo" é um
lote com `min_purchase_quantity = 2` e `max_purchase_quantity = 2`; o preço cadastrado
continua sendo o **unitário** e o comprador paga preço × quantidade. Trio, quarteto e combo
de grupo saem da mesma regra, sem código novo.

A compra gera **N ingressos independentes**, cada um com seu QR, transferíveis e estornáveis
separadamente. Nada muda na portaria, no atestado ou na contagem de público: um ingresso
continua sendo uma pessoa. Não existe campo de "quantas pessoas o ingresso admite" — ele
mudaria o significado de tudo o que conta gente.

Estorno parcial dentro de um combo é permitido: o mínimo é regra de **compra**, não vínculo
entre os ingressos depois de emitidos.

Lote cujo saldo não alcança o próprio mínimo **sai de venda** — sobrar 1 lugar num duplo
significa que aquele lote acabou, e continuar oferecendo levaria o comprador a uma recusa no
fim do checkout.

### Validação do estorno contra o gateway real

Todo o estorno foi construído e testado contra o `FakeGateway`. Três coisas só se
respondem observando uma devolução de verdade, e até lá vivem como remendo declarado no
código. **O caminho está instrumentado para a PRIMEIRA devolução real responder as três** —
sem precisar de um segundo experimento.

Faça uma venda de valor baixo e devolva metade, depois a outra metade, em sequência curta.
Então procure no log:

#### O que foi medido (sandbox, 28/08/2026)

**O Asaas não emite id de estorno.** `POST /v3/payments/{id}/refund` devolve o objeto da
COBRANÇA; as devoluções vivem em `refunds[]`, com os campos `dateCreated`, `description`,
`effectiveDate`, `endToEndIdentifier`, `refundedSplits`, `status`, `transactionReceiptUrl` e
`value`. Não há `refunds[].id`.

A consequência é boa: **a identidade de um estorno é a `description`** — o único campo que
nós controlamos, que sobrevive à ida e volta e distingue uma devolução parcial da outra. Já
é lá que a nossa chave viaja. Duas devoluções parciais de R$ 10 na mesma cobrança voltaram
como duas entradas distintas, cada uma com a sua chave.

Isso corrigiu **dois defeitos do cliente**, que liam o topo da resposta: o "id do estorno"
era o id da cobrança, e o "valor devolvido" era o valor total dela.

**O gateway serializa devoluções por cobrança.** Uma segunda devolução enquanto a primeira
processa é recusada com `invalid_object` / *"O estorno dessa cobrança já está em andamento"*.
Isso é ESPERA, não falha — virou `ErrRefundInProgress`, e o caminho manual do produtor
responde 409 pedindo para tentar em instantes, em vez de marcar a devolução como falha.

**O aviso (webhook) carrega a `description`?** Não deu para medir — a API do Asaas não expõe
histórico de entrega de webhook, e a máquina não tem túnel. Mas isso deixou de ser
bloqueante: o código passou a **reconhecer pelas duas vias**.

Se o aviso trouxer `payment.refunds[].description`, o eco é reconhecido de forma EXATA — cada
chave é a nossa, e basta ver se todas já são conhecidas; chave desconhecida significa
devolução feita por fora, e ela vale. Se o aviso não trouxer, cai na janela de 10 minutos,
que é palpite e por isso só entra quando não há alternativa. O log diz qual via foi usada:

    estorno: aviso reconhecido pela chave (eco do nosso)
    estorno: aviso tratado como eco pela JANELA (o gateway não mandou a chave)

A primeira devolução real em produção decide o assunto sem precisar de experimento: se a
linha da JANELA nunca aparecer, ela pode ser removida.

> **Chave por aplicação não separa webhook.** O webhook do Asaas é configurado por CONTA,
> não por chave de API: enquanto duas aplicações dividirem a conta, cada uma recebe os
> eventos da outra, inclusive estorno. Do nosso lado isso é seguro — cobrança fora do
> `payment_index` é reconhecida com 200 e não move nada (responder erro faria o gateway
> reenviar para sempre) —, mas vale saber que a separação real é por CONTA.

> **Atenção à configuração do webhook.** O webhook do sandbox desta conta está inscrito só em
> `PAYMENT_OVERDUE`, `PAYMENT_RECEIVED` e `PAYMENT_CONFIRMED` — **sem `PAYMENT_REFUNDED`**.
> Com essa lista, uma devolução feita pelo painel ou uma contestação nunca chegariam ao
> Timbre, e o ingresso continuaria válido com o dinheiro devolvido. Confira a inscrição de
> eventos do webhook de produção antes do primeiro evento real.

| Pergunta | Resposta | Consequência |
|---|---|---|
| O estorno tem id próprio? | **Não** — identidade é a `description` | cliente corrigido; conciliação por description |
| O gateway deduplica pela nossa chave? | **Não medido** — o replay bateu no bloqueio de concorrência antes | a idempotência segue sendo nossa |
| Marcadores reais de recusa? | `invalid_object` + "já está em andamento" | novo `ErrRefundInProgress`, retentável |
| O aviso traz a `description`? | **em aberto** | decide o fim da janela de eco |

#### Atalho: a sonda do sandbox

Antes de tocar em dinheiro real, `internal/payment/sandbox_probe_test.go` faz o mesmo
percurso contra o sandbox e imprime as respostas. Duas execuções, porque a cobrança precisa
ser marcada como recebida no painel (não há caminho de API documentado para isso aqui, e
inventar um daria um 404 que parece outra coisa):

A chave sai do painel do **sandbox** do Asaas — conta e login separados dos de produção.
Pode ir no ambiente ou no `.env` da raiz (a sonda lê dos dois, e diz de onde leu).

```bash
ASAAS_SANDBOX_KEY='<chave do sandbox>' \
  go test ./internal/payment/ -run TestSandboxRefundProbe -v   # cria a cobrança e para

# marque a cobrança como RECEBIDA no painel do sandbox, então:
ASAAS_SANDBOX_KEY='<chave>' ASAAS_PROBE_PAYMENT='<id impresso acima>' \
  go test ./internal/payment/ -run TestSandboxRefundProbe -v   # devolve e responde
```

Passar as variáveis na MESMA linha do comando evita o modo de falha mais comum: exportar num
terminal e rodar o teste noutro, e ler o "pulado" como se fosse defeito da sonda.

A sonda responde sozinha às perguntas 1 e 3 — ids distintos por devolução, replay da mesma
chave, e o texto da recusa quando os marcadores erram. A pergunta 2 (o **aviso** carrega o
id do estorno?) exige o servidor de pé com o webhook do sandbox apontado para ele: a
resposta está na linha `asaas: forma do aviso de estorno`.

As duas primeiras linhas registram só a **forma** do JSON — os caminhos de chave, sem os
valores. Payload de pagamento carrega dado do comprador; a estrutura, não.

Com as duas devoluções parciais em sequência, a pergunta que fecha o assunto é: dá para
distinguir uma da outra pelo que o aviso entrega? Se der, a janela de eco sai e a
conciliação vira por id. Se não der, ela fica — e o motivo passa a estar escrito aqui, como
decisão, em vez de suposição.

## Testes

```bash
make test   # go test -p 1 ./...  (exige TEST_DATABASE_URL; pula sem ela)
```

## Deploy

Coolify no VPS Sapienza, a partir do `Dockerfile` (build hermético `-mod=vendor`,
runtime distroless). Aponte `DATABASE_URL` para um database dedicado do Timbre. Ver
`SPEC.md` (arquitetura), `CLAUDE.md` (estrutura) e `AGENTS.md` (convenções).
