# Wager Service

[![ci](https://github.com/fiorellizz/backend-challenge-go/actions/workflows/ci.yml/badge.svg)](https://github.com/fiorellizz/backend-challenge-go/actions/workflows/ci.yml)

Serviço em Go que processa operações financeiras de provedores de jogos (`BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK`) sobre carteiras de jogadores, por HTTP e por SQS, com as mesmas garantias nas duas entradas e em múltiplas instâncias.

O enunciado completo está em [`docs/challenge.md`](docs/challenge.md). As decisões técnicas estão em [`ARCHITECTURE.md`](ARCHITECTURE.md); as evidências de execução distribuída em [`EVIDENCE.md`](EVIDENCE.md); o teste de carga em [`docs/LOAD_TEST.md`](docs/LOAD_TEST.md).

## Stack

| Responsabilidade | Tecnologia |
|---|---|
| Linguagem | Go 1.27 (`go.mod` e `Dockerfile`) |
| Composição | [Uber Fx](https://github.com/uber-go/fx) — um `fx.Module` por camada, ciclo de vida de servidor, workers e conexões |
| HTTP | `net/http` (roteador padrão com métodos e path values) |
| Persistência | PostgreSQL 16 via `pgx/v5`, SQL explícito, migrations com `golang-migrate` |
| Mensageria | AWS SQS (filas FIFO) via LocalStack |
| Autenticação | Keycloak (OIDC, `client_credentials`), validação com `go-oidc` |
| Observabilidade | `log/slog` JSON, Prometheus (`/metrics`), health checks |
| Ambiente | Docker Compose com três instâncias independentes da aplicação |

## Pré-requisitos

- Docker 24+ com Docker Compose v2
- Go 1.27 (para rodar testes e a aplicação fora do container)
- `make`, `curl`, `python3` (usados pelos alvos auxiliares do Makefile)

Portas usadas no host: `8080` (Keycloak), `8081`–`8083` (instâncias), `5433` (PostgreSQL — 5433 para não colidir com um PostgreSQL local), `4566` (LocalStack).

## Subindo tudo

```sh
docker compose up --build        # ou: make up (espera todos os serviços ficarem saudáveis)
```

O compose sobe, nesta ordem: PostgreSQL → `migrate` (aplica as migrations e sai) → LocalStack (o hook `deploy/localstack/init-queues.sh` cria as filas) → Keycloak (importa `deploy/keycloak/realm-export.json`) → `app-1`, `app-2`, `app-3`. Keycloak leva ~40 s; as instâncias esperam o discovery OIDC antes de aceitar tráfego.

```sh
make ps                          # estado dos containers
curl -s localhost:8081/health/ready   # {"checks":{"oidc":"ok","postgres":"ok","sqs":"ok"},"status":"ready"}
make smoke                       # abre uma carteira, envia uma aposta e reconcilia, com tokens reais
make down                        # derruba preservando o volume; make clean apaga tudo
```

## Variáveis de ambiente

Todas com valores locais em [`.env.example`](.env.example) (sem segredos reais). Para rodar fora do compose: `set -a; . ./.env.example; set +a; go run ./cmd/app`.

| Variável | Padrão | Uso |
|---|---|---|
| `APP_INSTANCE_ID` | `app` | nome da instância nos logs |
| `APP_ROLES` | `api,consumer,outbox` | papéis desta instância: `api` (rotas de negócio), `consumer` (consumidor SQS + worker de referências), `outbox` (publisher). Um binário, qualquer combinação |
| `HTTP_ADDR` | `:8080` | endereço do servidor HTTP (health e métricas sempre; rotas de negócio só com `api`) |
| `SHUTDOWN_TIMEOUT` | `20s` | prazo total do shutdown gracioso |
| `LOG_LEVEL`, `LOG_FORMAT` | `info`, `json` | `debug` mostra também os eventos do Fx |
| `DATABASE_URL`, `DATABASE_MAX_CONNS` | —, `10` | PostgreSQL |
| `AWS_REGION`, `AWS_ENDPOINT_URL`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | `us-east-1`, — | SQS; o endpoint aponta para o LocalStack |
| `SQS_WAGER_QUEUE_URL`, `SQS_WAGER_DLQ_URL`, `SQS_EVENTS_QUEUE_URL` | — | fila de entrada, sua DLQ e a fila de eventos de saída |
| `SQS_MAX_MESSAGES`, `SQS_WAIT_TIME_SECONDS`, `SQS_VISIBILITY_TIMEOUT_SECONDS` | `10`, `10`, `30` | consumo |
| `OIDC_ISSUER_URL` | — | `iss` esperado nos tokens (`http://localhost:8080/realms/wager`) |
| `OIDC_DISCOVERY_URL` | = issuer | onde buscar discovery/JWKS (`http://keycloak:8080/realms/wager` dentro do compose) |
| `OIDC_AUDIENCE`, `OIDC_INTERNAL_ROLE`, `OIDC_PROVIDER_ROLE`, `OIDC_PROVIDER_CLAIM` | `wager-api`, `internal-service`, `provider`, `provider_id` | modelo de permissões |
| `OUTBOX_BATCH_SIZE`, `OUTBOX_POLL_INTERVAL`, `OUTBOX_MAX_ATTEMPTS` | `100`, `500ms`, `10` | publisher |
| `REFERENCE_POLL_INTERVAL`, `REFERENCE_BASE_BACKOFF`, `REFERENCE_MAX_ATTEMPTS`, `REFERENCE_TTL` | `1s`, `2s`, `8`, `5m` | espera por referências de reversão |

A configuração é validada na inicialização; qualquer problema é listado de uma vez e o processo sai com código 2.

## Filas

`deploy/localstack/init-queues.sh` provisiona, quando o LocalStack fica pronto:

| Fila | Papel |
|---|---|
| `wager-transactions.fifo` | entrada de `WagerTransactionRequested`; visibility 30 s; redrive para a DLQ após 5 recebimentos |
| `wager-transactions-dlq.fifo` | mensagens inválidas ou esgotadas |
| `wager-events.fifo` | saída dos eventos de integração publicados pela outbox |

```sh
make queues                      # lista as filas e os atributos da fila principal
```

Exemplo de envio de uma operação pela fila (o `messageId` do envelope é a identidade durável; `MessageGroupId` = carteira, `MessageDeduplicationId` = `messageId`):

```sh
docker compose exec localstack awslocal sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --message-group-id "$WALLET_ID" --message-deduplication-id msg-1 \
  --message-body '{"messageId":"msg-1","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00Z","data":{"providerId":"provider-a","externalTransactionId":"t-1","idempotencyKey":"provider-a:t-1","playerId":"'$PLAYER_ID'","walletId":"'$WALLET_ID'","roundId":"round-1","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}}'
```

## Migrations

Versionadas em `migrations/` (`0001_init.up.sql` / `0001_init.down.sql`), aplicadas pelo serviço `migrate` do compose. Manualmente, com a infra de pé:

```sh
make migrate-up        # aplica
make migrate-down      # reverte a última
make migrate-reset     # reverte todas
make migrate-version   # versão atual
make migrate-create name=add_something
```

## Autenticação

O Keycloak importa o realm `wager` com os clientes de teste (`client_credentials`):

| Cliente | Secret | Role | Uso |
|---|---|---|---|
| `provider-a` | `provider-a-secret` | `provider`, claim `provider_id=provider-a` | envia operações e consulta as próprias transações |
| `provider-b` | `provider-b-secret` | `provider`, claim `provider_id=provider-b` | segundo provedor, para isolamento |
| `wallet-internal` | `wallet-internal-secret` | `internal-service` | abre e lê carteiras, reconcilia, consulta qualquer transação |
| `unauthorized-client` | `unauthorized-client-secret` | nenhuma | token válido sem permissão (403) |

```sh
make token                         # access token do provider-a
make token client=provider-b secret=provider-b-secret
make token-internal                # wallet-internal
```

Console do Keycloak: http://localhost:8080 (`admin` / `admin`).

## API

Todas as rotas de negócio exigem `Authorization: Bearer <token>`. `X-Correlation-ID` é aceito e devolvido em toda resposta.

| Rota | Quem | Respostas |
|---|---|---|
| `POST /wallets` | internal | `201` carteira; `409` já existe para `(playerId, currency)` |
| `GET /wallets/{walletId}` | internal | `200`; `404` |
| `GET /wallets/{walletId}/ledger?cursor=&limit=` | internal | `200` página com `nextCursor` opaco (limite 50, máx. 200) |
| `POST /wallets/{walletId}/reconciliation` | internal | `200` comparação saldo × ledger; não altera nada |
| `POST /wagering/transactions` (header `Idempotency-Key` obrigatório) | provider (body `providerId` = claim do token) | `200 PROCESSED`, `202 PENDING_REFERENCE`, `422 REJECTED` com `failureCode`, `409` conflito de idempotência |
| `GET /wagering/transactions/{transactionId}` | provider (só as próprias; outras → 404) ou internal | `200`; `404` |
| `GET /providers/{providerId}/wagering/transactions/{externalTransactionId}` | provider (path = claim) | `200`; `403`; `404` |
| `GET /health/live`, `GET /health/ready`, `GET /metrics` | público | |

Erros têm sempre a forma `{"error":{"code":"...","message":"..."}}` com códigos `VALIDATION_ERROR` (400), `UNAUTHENTICATED` (401), `FORBIDDEN` (403), `NOT_FOUND` (404), `CONFLICT` (409), `REJECTED` (422), `UNAVAILABLE` (503, dependência fora — reenvie com backoff), `INTERNAL_ERROR` (500).

### Exemplo completo

```sh
INTERNAL=$(make -s token-internal); PROVIDER=$(make -s token)
PLAYER=$(python3 -c 'import uuid; print(uuid.uuid4())')

# abertura (cria OPENING, lançamento de crédito e dois eventos no mesmo commit)
curl -s -X POST localhost:8081/wallets -H "Authorization: Bearer $INTERNAL" -H 'Content-Type: application/json' \
  -d '{"playerId":"'$PLAYER'","initialBalance":{"amount":"1000.00","currency":"BRL"}}'
# {"id":"<walletId>","playerId":"...","balance":{"amount":"1000.00","currency":"BRL"},"version":1,...}

# aposta
curl -s -X POST localhost:8081/wagering/transactions -H "Authorization: Bearer $PROVIDER" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: provider-a:transaction-123' \
  -d '{"providerId":"provider-a","externalTransactionId":"transaction-123","playerId":"'$PLAYER'","walletId":"<walletId>","roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}'
# {"transactionId":"...","status":"PROCESSED","balance":{"amount":"975.00","currency":"BRL"},"idempotentReplay":false}

# reenvio idêntico → mesmo resultado, idempotentReplay:true; mesmo Idempotency-Key com outro corpo → 409

# reversão (acrescente referenceExternalTransactionId); se a aposta ainda não chegou → 202 PENDING_REFERENCE
curl -s -X POST localhost:8081/wagering/transactions -H "Authorization: Bearer $PROVIDER" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: provider-a:refund-1' \
  -d '{"providerId":"provider-a","externalTransactionId":"refund-1","playerId":"'$PLAYER'","walletId":"<walletId>","roundId":"round-987","gameId":"fortune-chimp","kind":"REFUND","money":{"amount":"25.00","currency":"BRL"},"referenceExternalTransactionId":"transaction-123"}'

# reconciliação
curl -s -X POST localhost:8081/wallets/<walletId>/reconciliation -H "Authorization: Bearer $INTERNAL"
```

## Testes

```sh
go test ./...            # unitários (domínio, casos de uso com store em memória, handlers, config) — sem infra
go test -race ./...      # os mesmos com o detector de corrida
go vet ./...
make check               # gofmt -l, vet, test, race
```

### Integração (infra real)

Exigem PostgreSQL, LocalStack e Keycloak de pé. Os testes sobem a **aplicação dentro do próprio processo de teste** (mesmos módulos Fx do `main`), então as instâncias `app-*` do compose devem estar paradas para não disputar as filas — o alvo cuida disso.

```sh
make infra                       # só as dependências (ou make up; o alvo abaixo para os app-*)
make test-integration            # -tags=integration: ./internal/... e ./test/integration/...
make test-integration-race
```

O que a suíte `test/integration` cobre: migrations e constraints (trigger do ledger, saldo negativo, índice parcial de reversão única); 50 replays paralelos → um débito; disputa de duas apostas de 80.00 sobre 100.00 e reenvio; carteiras distintas em paralelo; todas as reversões; REFUND antes da BET resolvido pelo worker; expiração → `REFERENCE_NOT_FOUND`; autenticação, roles e isolamento entre provedores com tokens reais; reinício com nova instância (idempotência, pendência retomada, reconciliação); composição Fx em cada perfil de papéis; a mesma operação por HTTP e SQS; reentrega e DLQ; dois publishers disputando a outbox. Os testes de cada adapter (`internal/adapter/*/*_integration_test.go`) cobrem repositórios, verifier OIDC, publisher e consumer isoladamente.

### Evidências com múltiplas instâncias

```sh
make evidence                    # sobe tudo, roda ./test/evidence (-tags=evidence) e escreve EVIDENCE.md
```

Cenários contra `app-1/2/3` reais: 50 replays e disputa 80+80 distribuídos entre instâncias; `SIGKILL` no consumidor com 200 mensagens em voo; `SIGKILL` no publisher da outbox; `restart` com reversão pendente; PostgreSQL pausado 12 s sob tráfego; `SIGTERM` sob carga. Veja [`EVIDENCE.md`](EVIDENCE.md).

### Carga

```sh
make load                        # k6 em Docker contra as três instâncias; VUS=, HOLD=, WALLETS= opcionais
```

Metodologia e números em [`docs/LOAD_TEST.md`](docs/LOAD_TEST.md).

## Layout

```
cmd/app                 binário único; papéis por APP_ROLES
internal/domain         money, wallet, wagering, event, id, errs — só stdlib
internal/usecase        casos de uso e portas (interfaces); usecasetest tem o store em memória
internal/adapter        postgres, httpapi, oidc, sqsmsg — implementam as portas
internal/worker         outbox publisher, reference resolver
internal/platform       config, logging, metrics, auth
internal/fxmodules      a fiação: um fx.Module por camada
migrations, deploy      schema; localstack, keycloak, k6
test/integration        suíte com infra real; test/evidence: cenários multi-instância
```
