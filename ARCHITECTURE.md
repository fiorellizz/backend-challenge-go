# Arquitetura e decisões

Este documento registra as decisões do serviço e o porquê de cada uma. Onde uma decisão foi provada por teste ou evidência, o teste é citado. As limitações e interpretações estão no final.

## 1. Visão geral

```
              HTTP (net/http)                         SQS FIFO (LocalStack)
   provider ──────────────┐                 ┌──────────── wager-transactions.fifo
   internal ──────────────┤                 │                     │ redrive (5x)
                          ▼                 ▼                     ▼
                 ┌─────────────────────────────────┐    wager-transactions-dlq.fifo
                 │  usecase.WageringService.Process │
                 │  (uma transação SQL por operação) │
                 └────────────────┬────────────────┘
                                  ▼
   PostgreSQL:  wallets · wager_transactions · wallet_ledger_entries · inbox_messages · outbox_events
                                  │
              ┌───────────────────┴──────────────────┐
              ▼                                      ▼
   worker: reference resolver              worker: outbox publisher ──► wager-events.fifo
   (PENDING_REFERENCE, backoff)            (FOR UPDATE SKIP LOCKED, backoff)
```

Um binário, três papéis (`APP_ROLES=api,consumer,outbox`). O compose sobe três instâncias com os três papéis: qualquer uma atende HTTP, consome a fila, resolve referências e publica eventos. Nada depende de existir exatamente um processo — provado em [`EVIDENCE.md`](EVIDENCE.md).

Camadas (a dependência aponta sempre para dentro):

| Camada | Pacotes | Regra |
|---|---|---|
| Domínio | `internal/domain/{money,wallet,wagering,event,id,errs}` | só stdlib; sem Fx, pgx, HTTP, SQS |
| Casos de uso | `internal/usecase` | orquestra o domínio; **declara as portas** (`ports.go`) que consome |
| Adapters | `internal/adapter/{postgres,httpapi,oidc,sqsmsg}` | implementam as portas |
| Workers | `internal/worker` | loops de fundo sobre os casos de uso |
| Plataforma | `internal/platform/{config,logging,metrics,auth}` | infra transversal |
| Composição | `internal/fxmodules` | único lugar que conhece o grafo inteiro |

## 2. Dinheiro

**Representação:** `money.Money{minor int64, currency Currency}` — centavos em `int64`, escala fixa de duas casas, moeda ISO 4217 (três letras maiúsculas) carregada no próprio valor. Nunca `float`: o parser é manual (`strings.Cut` no ponto, dígitos ASCII, `strconv.ParseInt` em cada parte) e a serialização é `{"amount":"25.00","currency":"BRL"}` com `amount` sempre string; um número JSON é rejeitado.

**Limites:** ±92 233 720 368 547 758,07. Overflow é erro explícito (`money.ErrOverflow`) no parsing, em `Add`, `Sub` e `Neg` — checado antes da operação, nunca *wrap*.

**Entradas externas:** aceitas `d+`, `d+.d` e `d+.dd`; `"25"` e `"25.5"` são formas equivalentes de `"25.00"` e `"25.50"` e são **normalizadas antes do hash de idempotência** (o hash usa `Amount()`, sempre com duas casas). Rejeitados sem arredondar: vazio, sinal, espaço, `NaN`, `Infinity`, notação científica, três ou mais casas, vírgula, dígitos não ASCII. Negativo é aceito só internamente (`money.New`, `ParseSigned`) para a diferença da reconciliação; nunca em saldo.

**Persistência:** `BIGINT` em centavos + `CHAR(3)`, preservando valor e moeda exatamente. O `Money` zero (`Money{}`) é rejeitado por todos os métodos (`ErrUninitialized`).

Testes: `internal/domain/money` (100 % de cobertura, 16 entradas inválidas, limites exatos do `int64`).

## 3. Modelo de domínio

- **`wallet.Wallet`** é a raiz do agregado: estado privado, `New` (cria, versão 1, `fresh=true`) separado de `Rehydrate` (valida invariantes, não reaplica nada). O saldo só muda por `OpeningCredit` (uma vez, mantém versão 1), `Credit` e `Debit` (incrementam a versão); cada um devolve o `LedgerEntry` que precisa ser persistido no mesmo commit. `Debit` abaixo de zero devolve `ErrInsufficientBalance` e deixa o agregado intacto.
- **`wallet.LedgerEntry`** só nasce por `NewLedgerEntry`, que recalcula `balanceAfter = balanceBefore ± amount` e rejeita divergência — a mesma regra que o `CHECK ledger_balance_arithmetic` do banco.
- **`wagering.WagerTransaction`** guarda o estado num `Snapshot` privado; construtores `NewExternal` (política de valor por tipo, `OPENING` recusado, reversão exige referência, só `WIN` aceita referência opcional) e `NewOpening`; `Rehydrate` valida a forma (externo com todos os metadados, interno sem nenhum — espelho do `CHECK wager_tx_origin_shape`).
- **Erros:** quatro sentinelas em `errs` (`ErrValidation`, `ErrConflict`, `ErrNotFound`, `ErrTransient`) envolvidas com `%w`; rejeição de negócio é o tipo `wagering.Rejection{Code}` (via `errors.As`). `panic` nunca representa rejeição.

## 4. Máquina de estados e códigos de falha

```
PENDING ──► PROCESSED | REJECTED | FAILED
   │
   ▼ (reversão sem referência)
PENDING_REFERENCE ──(retry, attempts++)──► PROCESSED | REJECTED
```

Terminal nunca transiciona (`ErrTerminalState`, testado contra todas as transições). Como o processamento é **síncrono numa única transação SQL**, `PENDING` nunca é commitado sozinho: ou a operação termina no mesmo commit, ou vira `PENDING_REFERENCE`. Por isso não há worker de retomada de `PENDING` — a única pendência durável é a referência, e ela tem seu worker.

| Situação | Classificação | Efeito |
|---|---|---|
| Postgres/SQS fora, deadlock, timeout | **transitória** (`errs.ErrTransient`) | transação SQL aborta; HTTP `503`; mensagem SQS volta à fila com backoff |
| regra de negócio (saldo, referência, moeda) | **rejeição definitiva** (`REJECTED` + `failureCode`) | commitada, evento `WagerTransactionRejected`, mensagem removida |
| entrada inválida (JSON, tipo, valor, ids) | **corrigível** (`400` / DLQ) | nada é persistido |
| conflito de idempotência | **definitiva** (`409` / DLQ) | nada é persistido |

`failureCode` (constantes em `wagering/kind.go`):

| Código | Quando |
|---|---|
| `INSUFFICIENT_BALANCE` | `BET` acima do saldo |
| `REVERSAL_INSUFFICIENT_BALANCE` | `ROLLBACK` de `WIN`/`REFUND` que debitaria além do saldo — código distinto para auditoria |
| `REFERENCE_NOT_FOUND` | referência nunca chegou dentro do orçamento de tentativas/TTL |
| `REFERENCE_NOT_PROCESSED` | referência existe mas não está `PROCESSED` (pendente, rejeitada ou falha) |
| `REFERENCE_MISMATCH` | provedor, jogador, carteira, rodada, valor ou tipo incompatíveis (ex.: `REFUND` de `WIN`) |
| `REFERENCE_ALREADY_REVERSED` | a referência já tem uma reversão bem-sucedida |
| `CURRENCY_MISMATCH` | moeda da operação ≠ moeda da carteira |
| `PLAYER_MISMATCH` | jogador informado não é o dono da carteira |
| `PERMANENT_FAILURE` | falha permanente de infraestrutura registrada como `FAILED` |

## 5. Regras por tipo e reversões

| Tipo | Movimento | Valor | Ledger | Versão |
|---|---|---|---|---|
| `OPENING` (interno) | crédito | > 0 | sim | fica em 1 |
| `BET` | débito | > 0 | sim | +1 |
| `WIN` | crédito | > 0 | sim | +1 |
| `LOSS` | nenhum | = 0 | não | não muda; emite `WagerTransactionProcessed` sem `WalletBalanceChanged` |
| `REFUND` | crédito | = valor da `BET` referenciada | sim | +1 |
| `ROLLBACK` | inverso da referência (`BET`→crédito, `WIN`/`REFUND`→débito) | = valor referenciado | sim | +1 |

**Uma referência aceita uma única reversão bem-sucedida**, seja `REFUND` ou `ROLLBACK` — mais restritivo que "uma de cada", de propósito: `REFUND` seguido de `ROLLBACK` da mesma `BET` devolveria o mesmo débito duas vezes. Imposto em duas camadas: pré-checagem `HasSuccessfulReversal` sob o lock da carteira (para poder responder `REFERENCE_ALREADY_REVERSED` em vez de abortar a transação) e o índice parcial único `wager_tx_single_successful_reversal` como rede final. Reversões **rejeitadas** não ocupam a vaga. Reversão parcial não existe.

Referência existente mas ainda `PENDING_REFERENCE`, `REJECTED` ou `FAILED` → `REFERENCE_NOT_PROCESSED`, rejeição definitiva: não há o que desfazer, e o provedor pode reenviar com outra chave depois que a referência se resolver.

Um `WIN` pode nomear uma aposta da mesma rodada; se ela não existir, o `WIN` processa normalmente (a referência é informativa).

## 6. A transação central

`usecase.WageringService.Process` (HTTP e SQS chamam exatamente o mesmo método; SQS passa um `InboxMark`):

```
BEGIN (READ COMMITTED)
  [SQS] inbox.Find(consumer, messageId)  → já tratada? replay do resultado (hash diferente → poison)
  wallets.Get(walletId)                  → 404 sem persistir nada
  INSERT wager_transactions ... ON CONFLICT DO NOTHING
     └─ não inseriu: carrega por chave → hash igual: replay | hash ≠: 409 | chave nova mas (provider, external) existe: 409
  SELECT wallets ... FOR NO KEY UPDATE   → serialização por carteira
  resolve referência (REFUND/ROLLBACK/WIN) → ausente: PENDING_REFERENCE + evento, COMMIT
  CheckWallet + Effect + Debit/Credit    → rejeição vira REJECTED, não erro
  INSERT ledger · UPDATE wallets (WHERE version = esperada) · UPDATE wager_transactions · INSERT outbox
  [SQS] INSERT inbox_messages
COMMIT
  [SQS] DeleteMessage
```

Delimitação: o caso de uso recebe `Repositories` amarrados à transação por `UnitOfWork.WithinTx`; os repositórios nunca abrem transação. Leituras usam o mesmo `Repositories` amarrado ao pool. A reconciliação usa `WithinSnapshot` (REPEATABLE READ, somente leitura).

A ordem **insert da transação antes do lock da carteira** faz duplicatas concorrentes esperarem no índice único (`idempotency_key`), não no lock — a segunda vê `inserted=false` assim que a primeira commita e cai no replay.

## 7. Idempotência

Persistente, em três camadas no banco: índice único em `idempotency_key`; índice único em `(provider_id, external_transaction_id)` (a mesma operação não pode ser reaplicada com outra chave → `409`); `UNIQUE (wallet_id, transaction_id)` no ledger como rede final contra movimentação duplicada.

**Hash do payload:** SHA-256 do JSON canônico — objeto com chaves ordenadas (`encoding/json` sobre `map`), sem espaços — de `providerId, externalTransactionId, playerId, walletId, roundId, gameId, kind, money{amount, currency}` e `referenceExternalTransactionId` quando presente. `amount` normalizado para duas casas. A chave de idempotência, `messageId`, `correlationId` e headers ficam fora. HTTP e SQS produzem o mesmo hash para a mesma operação (testado em `TestSameOperationOverHTTPAndSQS`).

**Replay:** devolve o resultado persistido com `idempotentReplay: true`, inclusive `PENDING_REFERENCE` e `REJECTED`, e com o **saldo observado no processamento original** (coluna `balance_after_minor`), mesmo que a carteira já tenha mudado. `Idempotency-Key` é obrigatório e nunca substituído por um calculado.

## 8. Concorrência e locks

**Pessimista por carteira:** `SELECT ... FOR NO KEY UPDATE` na linha de `wallets`, sem retry. Carteiras diferentes não se tocam (120 operações em 12 carteiras por 3 instâncias em 420 ms — `EVIDENCE.md`). Nenhum lock global, mutex de processo ou advisory lock.

**Por que `NO KEY UPDATE` e não `FOR UPDATE`:** o `INSERT` em `wager_transactions` tem chave estrangeira para `wallets`, e o PostgreSQL toma `KEY SHARE` na carteira nesse insert. Com `FOR UPDATE`, duas operações na mesma carteira ficavam cada uma segurando `KEY SHARE` e esperando a outra soltar — deadlock real, encontrado por `TestDistinctWalletsProceedInParallel`. `NO KEY UPDATE` é compatível com `KEY SHARE`, conflita consigo mesmo e com `UPDATE`, e é exatamente o lock que uma alteração de saldo precisa, já que a chave primária não muda.

**Lost update:** além do lock, `UPDATE wallets ... WHERE version = $esperada` — se alguém alterou a linha, zero linhas afetadas → `ErrConflict`. Cinto e suspensórios. A versão é o token exposto ao cliente e carregado em `WalletBalanceChanged`.

**No banco, independentemente do código:** `CHECK (balance_minor >= 0)`, triggers que proíbem `UPDATE`/`DELETE`/`TRUNCATE` no ledger, `CHECK` da aritmética do lançamento, unicidade de `(player_id, currency)`, `(wallet_id, transaction_id)` e uma única `OPENING` por carteira — todos exercitados em `TestSchemaEnforcesFinancialInvariants`.

## 9. Referências pendentes

Reversão cuja referência não existe → `PENDING_REFERENCE` com `next_reference_attempt_at` e `reference_deadline_at`, evento `WagerTransactionPendingReference`, e, no SQS, a mensagem é concluída (inbox + delete): o worker assume. `ReferencePolicy` no domínio: backoff `base × 2^tentativa` limitado ao TTL; fim por `REFERENCE_MAX_ATTEMPTS` (8) ou `REFERENCE_TTL` (5 min), o que vier primeiro.

Worker `ReferenceResolver` em toda instância com papel `consumer`: uma transação por referência — `ClaimNextPendingReference` (`FOR UPDATE SKIP LOCKED`), lock da carteira, e ou o **mesmo `settle`** do caminho síncrono (mesmas regras e códigos), ou `RetryReference` (agenda), ou `REJECTED` com `REFERENCE_NOT_FOUND` + evento. Queda no meio → abort libera o claim, outra instância retoma (`TestRestartWithPendingReference`). Replay durante a espera devolve `202` com o estado parado.

## 10. Inbox e outbox

**Inbox (`inbox_messages`):** identidade durável = `messageId` do envelope + nome do consumidor; hash SHA-256 do corpo bruto. A linha é gravada **na mesma transação** do resultado, então existe se e somente se o tratamento foi commitado. Reentrega: linha encontrada com hash igual → replay; hash diferente → poison (DLQ). O delete na fila vem só depois do commit; queda entre os dois é o caso coberto por `TestConsumerKilledWhileProcessing` (200 mensagens, 200 linhas na inbox, 200 débitos).

**Outbox (`outbox_events`):** o evento é gravado na transação do fato, com o payload serializado na hora (snapshot imutável). O `OutboxPublisher` (papel `outbox`) reivindica até 100 linhas com `FOR UPDATE SKIP LOCKED`, publica (`SendMessageBatch`, chunks de 10 em paralelo) e marca `published_at` num único `UPDATE ... WHERE event_id = ANY($1)` — tudo na mesma transação. Publicar **antes** de commitar a marcação é deliberado: queda entre os dois republica com o **mesmo `eventId`** (SQS deduplica em 5 min; consumidores deduplicam por `eventId` além disso). Claim abandonado é liberado pelo abort, sem reaper. Falha de um evento não afeta os outros: `attempts+1`, próximo em `1s × 2^(n−1)` até 5 min. Depois de `OUTBOX_MAX_ATTEMPTS` o evento **não é descartado** — só o log sobe para ERROR: um evento confirmado no banco nunca se perde.

**Fila de saída `wager-events.fifo`:** corpo = envelope (`eventId, eventType, aggregateType, aggregateId, correlationId, causationId, occurredAt, version, data`); `MessageGroupId = aggregateId` (ordem por carteira/transação); `MessageDeduplicationId = eventId`; atributos `eventType` e `eventId` para filtrar sem parsear. Tipo e versão são fixados pelos construtores em `domain/event`; timestamps UTC RFC 3339; dinheiro como strings.

## 11. Consumidor SQS

- Long polling (`SQS_WAIT_TIME_SECONDS=10`, até 10 mensagens), visibility timeout 30 s; cada mensagem é tratada com um contexto **desacoplado do cancelamento** e limitado a `visibility − 2 s`.
- Contrato para produtores: `MessageGroupId = walletId` (ordem por carteira; o banco continua sendo a garantia), `MessageDeduplicationId = messageId`.
- Decisões: `Ack` (PROCESSED, REJECTED e PENDING_REFERENCE são resultados commitados → delete); `Reject` (JSON inválido, sem `messageId`, tipo desconhecido, `OPENING`, carteira inexistente, conflito de idempotência → cópia para a DLQ com atributo `reason` + delete); `Retry` (transitório → `ChangeMessageVisibility` com backoff 5 s, 10 s, 20 s, 40 s… até 5 min; o redrive da fila manda à DLQ após 5 recebimentos).
- `SIGTERM`: o loop não inicia outro poll; um poll em curso termina (≤ `WaitTimeSeconds`) e tudo que ele devolveu é processado até o fim. Cancelar o poll deixaria mensagens já entregues invisíveis até o visibility timeout — o mesmo efeito de um crash — e foi assim que a suíte de integração pegou essa decisão errada na primeira versão.

## 12. Autenticação e autorização

IdP externo (Keycloak) com `client_credentials`; o serviço nunca emite tokens nem guarda senhas. `go-oidc` faz discovery no `OnStart` (com retry enquanto o Keycloak sobe), valida assinatura (JWKS), `iss`, `aud=wager-api` e expiração. O motivo da recusa é logado, nunca devolvido.

**Issuer:** o Keycloak escreve em `iss` o host pelo qual foi chamado. `KC_HOSTNAME=http://localhost:8080` fixa o issuer e `KC_HOSTNAME_BACKCHANNEL_DYNAMIC=true` faz o discovery consultado por `keycloak:8080` devolver o JWKS nesse host. No serviço, `OIDC_ISSUER_URL` (o que o token carrega) e `OIDC_DISCOVERY_URL` (onde buscar chaves) são separados.

**Modelo:** duas identidades por *realm role*.

| Role | Claim | Pode |
|---|---|---|
| `provider` | `provider_id` obrigatório | `POST /wagering/transactions` (body `providerId` = claim, senão 403), `GET /providers/{seu id}/...`, `GET /wagering/transactions/{id}` só das próprias (outras → 404, para não revelar ids) |
| `internal-service` | — | carteiras, ledger, reconciliação, qualquer transação por id |

Token válido sem role → 403 em tudo; health e métricas públicos. Isolamento provado com tokens reais em `TestAuthenticationAndProviderIsolation` (inclusive replay da chave de `provider-a` por `provider-b` → 403, e token adulterado → 401).

**Broker:** o acesso ao SQS é por credenciais do ambiente (no LocalStack, `test/test`); em produção, políticas IAM por fila. As validações de domínio no consumidor não dependem disso.

## 13. Uber Fx e ciclo de vida

- `fxmodules.App(cfg)` devolve as opções usadas **pelo `main` e pelos testes de composição** (`fxtest.New(t, App(cfg)...)`) — o que é testado é o que roda.
- Config carregada **antes** do container (`config.Load(os.Getenv)`, valida tudo e lista todos os problemas de uma vez) e injetada com `fx.Supply`, porque `fx.StopTimeout(cfg.ShutdownTimeout)` e o logger precisam dela antes do grafo existir.
- Um `fx.Module` por camada (`platform`, `postgres`, `sqs`, `oidc`, `usecase`, `workers`, `httpapi`). Construtores devolvem structs concretas; interfaces via `fx.Annotate(..., fx.As(new(usecase.UnitOfWork)))`.
- **Value group `readiness`:** cada adapter contribui um `ReadinessCheck` com `fx.ResultTags`; `/health/ready` os recebe com `fx.ParamTags` sem conhecer Postgres, SQS ou OIDC.
- Hooks de recursos registrados **dentro dos construtores** (pool, verifier OIDC): rodam antes de qualquer componente que dependa deles, então param depois. Workers usam `runLoop`: `OnStart` sobe a goroutine e retorna; `OnStop` cancela o contexto, espera o `WaitGroup` e respeita o `stopCtx`. Papéis decidem quais `fx.Invoke` de worker entram.
- Ordem de shutdown = inversa da inicialização, e o módulo HTTP é listado por último para **parar primeiro**. Observado no log: `http server shutting down → sqs consumer stopped → outbox publisher stopped → reference resolver stopped → postgres pool closed`.
- Eventos do próprio Fx vão para o `slog` JSON (`fxevent.SlogLogger`) em DEBUG.

## 14. Resiliência a dependências

- Timeout de **10 s por requisição HTTP** (middleware): um Postgres congelado vira `503` e devolve a conexão ao pool em vez de esgotá-lo — sem isso, uma pausa de 3 s no banco levou uma mensagem SQS à DLQ (cenário "PostgreSQL indisponível" em `EVIDENCE.md`).
- Timeout de 30 s por iteração nos workers; `ConnectTimeout` de 5 s no pool; retry no `Ping` e no discovery OIDC durante o boot.
- Erros de banco classificados por SQLSTATE: `23505` → conflito; `40001`/`40P01` e classes `08`/`53`/`57` → transitório.

## 15. Reconciliação e observabilidade

`POST /wallets/{id}/reconciliation` recalcula `SUM(CREDIT) − SUM(DEBIT)` do ledger (incluindo a abertura) e compara com o saldo numa transação REPEATABLE READ somente leitura — saldo e soma vêm da mesma foto, então uma operação concorrente não gera falso positivo. `difference = stored − calculated`; divergência vai para a resposta, para o log (`WARN`) e para `reconciliation_divergences_total`. Nada é escrito.

Logs JSON com `correlationId` (do header `X-Correlation-ID` ou gerado, devolvido na resposta e propagado até os eventos), `messageId`, `transactionId`, `walletId`, `providerId`; nunca token, header de autorização ou payload financeiro completo. Métricas em `/metrics`: resultados por status/tipo/origem, duplicatas, conflitos, latência, resoluções de referência, outbox (publicados, falhas, **atraso medido por relógio** — a primeira versão só atualizava o gauge quando o publisher ficava ocioso, e o teste de carga mostrou 76 mil eventos atrasados com o gauge em zero), decisões do consumidor, DLQ, divergências, HTTP por rota. Nenhum id vira label.

## 16. Contratos HTTP

Códigos: `200` processado/replay · `201` carteira · `202` `PENDING_REFERENCE` · `400 VALIDATION_ERROR` · `401 UNAUTHENTICATED` · `403 FORBIDDEN` · `404 NOT_FOUND` · `409 CONFLICT` (chave reutilizada com outro corpo, mesma operação sob outra chave, carteira duplicada) · `422 REJECTED` (com `failureCode` e o `transactionId`) · `503 UNAVAILABLE` (transitório; reenviar com backoff) · `500 INTERNAL_ERROR`. Corpo de erro sempre `{"error":{"code","message"}}`. Paginação do ledger por cursor opaco (base64url do `seq`, atribuído sob o lock da carteira — ordem estável).

## 17. Limitações e interpretações

- **Processamento síncrono:** não há commit intermediário de `PENDING` nem worker de retomada de `PENDING`; a única pendência durável é `PENDING_REFERENCE`. O enunciado permite ("operações sem dependências podem ser concluídas de forma síncrona").
- **Uma reversão por referência**, não uma de cada tipo (ver §5). Referência não `PROCESSED` é rejeição definitiva.
- **Só BRL** nos cenários; o tipo carrega a moeda e há testes de incompatibilidade, mas não existe câmbio nem tabela de moedas.
- **Sem autorização do broker por política:** no LocalStack o acesso é por credenciais fake; a integração com IAM/policies por fila fica para o ambiente real.
- **Mensagens inválidas não geram linha na inbox** (não há o que registrar sem `messageId` confiável); vão à DLQ com `reason`.
- **Republicação de eventos pode duplicar** fora da janela de 5 min do SQS; consumidores devem deduplicar por `eventId`.
- **Teste de carga** roda tudo no mesmo host e contra o LocalStack, que limita o throughput da outbox (atraso de até 14 s no pico); ver `docs/LOAD_TEST.md`.
- **Não feito:** tracing OpenTelemetry, dashboards, ledger de partidas dobradas (diferenciais opcionais).
