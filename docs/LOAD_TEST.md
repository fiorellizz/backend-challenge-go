# Teste de carga

Resultado de `make load` contra as três instâncias do Docker Compose. O objetivo não é bater uma meta de RPS, e sim mostrar como o serviço se comporta sob concorrência real — na mesma carteira e entre carteiras — e o que acontece com a outbox quando a entrada é mais rápida que a publicação.

## Comando reproduzível

```sh
make up                      # postgres, localstack, keycloak, app-1..3
make load                    # padrão: 60 VUs, 20s de rampa + 60s de platô + 10s de descida
make load VUS=100 HOLD=2m    # variações
```

O script é [`deploy/k6/wager.js`](../deploy/k6/wager.js), executado pela imagem `grafana/k6:0.54.0` em rede de host. Ele mesmo obtém os tokens no Keycloak (`client_credentials`), abre as carteiras, gera o tráfego e, no `teardown`, reconcilia todas as carteiras — o teste falha se alguma divergir.

## Ambiente

| Item | Valor |
|---|---|
| Máquina | 8 vCPUs, 14 GB RAM, Linux 7.0, tudo no mesmo host (k6, 3 instâncias, PostgreSQL 16, LocalStack 3.8, Keycloak 26) |
| Aplicação | Go 1.27, imagem do `Dockerfile`, 3 instâncias com `APP_ROLES=api,consumer,outbox`, `DATABASE_MAX_CONNS=10` cada |
| Distribuição | k6 alterna as instâncias por iteração (`:8081`, `:8082`, `:8083`) |
| Carteiras | 40, saldo inicial 100000.00 BRL; cada VU é fixado a uma carteira (`__VU % 40`), então ~1,5 VUs disputam a mesma linha o tempo todo |

## Metodologia

Mix por iteração (escolha aleatória por VU, `sleep` de 50ms entre iterações):

| Operação | Peso | Observação |
|---|---|---|
| `BET` 1.00–5.00 | 60% | débito sob lock da carteira |
| `WIN` 1.00–5.00 | 20% | crédito |
| `LOSS` 0.00 | 5% | sem movimentação, sem ledger |
| `REFUND` da última BET do VU | 5% | resolve referência; a segunda tentativa sobre a mesma BET é rejeitada com `REFERENCE_ALREADY_REVERSED` |
| replay da última BET (mesma chave) | 10% | exercita a idempotência persistente |

Só 5xx e erros de transporte contam como falha (`http.expectedStatuses(200, 201, 202, 409, 422)`); 422 e 409 são resultados de negócio contabilizados em contadores próprios. Um VU extra lê `/metrics` de cada instância a cada 2s e registra `outbox_lag_seconds`. O atraso da outbox também foi medido no banco (`published_at − occurred_at`) na janela do teste.

## Resultados (60 VUs, 90s)

### HTTP

| Métrica | Valor |
|---|---|
| Requisições | 63 928 (**657 req/s**) |
| Falhas (5xx / transporte) | **0** (`http_req_failed = 0.00%`) |
| Latência de `POST /wagering/transactions` — p50 | 15,5 ms |
| p90 | 38,9 ms |
| p95 | 47,6 ms |
| p99 | 66,4 ms |
| máx | 158,5 ms |

### Resultados de negócio

| Contador | Valor |
|---|---|
| `wager_processed` (200, `idempotentReplay=false`) | 57 098 |
| `wager_replays` (200, `idempotentReplay=true`) | 6 359 |
| `wager_rejected` (422) | 245 — todas `REFERENCE_ALREADY_REVERSED` (segundo REFUND sobre a mesma BET) |
| `wager_conflicts` (409) | 0 |
| `wager_pending_reference` (202) | 0 (as referências já existiam) |
| Reconciliação ao final | **40/40 carteiras consistentes** |

Do lado do servidor, `wager_transactions_total` acumulado nas três instâncias bateu com o k6: 45 389 BET, 13 131 WIN, 3 143 LOSS e 2 985 REFUND processados; 246 REFUND rejeitados.

### Outbox

| Métrica | Valor |
|---|---|
| Eventos gerados na janela | 111 350 (~1 200 eventos/s no platô) |
| Eventos pendentes ao final do teste | **0** |
| Tentativas por evento (máx) | 1 |
| Atraso publicação − ocorrência: p50 | 8,3 s |
| p99 | 14,2 s |
| máximo | 14,3 s |
| `outbox_lag_seconds` na última amostra | 0 |

## Leitura dos números

- **Concorrência na mesma carteira não gera erro nem perda.** Com ~1,5 VUs por carteira disputando a linha (`FOR NO KEY UPDATE`), a latência p99 fica em 66 ms e a reconciliação de todas as carteiras fecha. Os 6 359 replays devolveram o resultado persistido sem tocar no saldo.
- **A outbox atrasa no pico e recupera.** A entrada gera ~1 200 eventos/s; os três publishers (lotes de 100, `SendMessageBatch` de 10 em paralelo) publicam ~1 000–1 300/s contra o SQS do LocalStack, que é a limitação do ambiente (implementação Python num único container). O atraso cresce até ~14 s no platô e volta a zero quando a carga cai; nenhum evento se perde e nenhum precisa de segunda tentativa. Em SQS real, ou com `OUTBOX_BATCH_SIZE` maior, a folga é maior.
- **Nenhum 5xx em 64 mil requisições**, com timeout por requisição de 10 s e pool de 10 conexões por instância.

## Limitações do teste

- Tudo roda no mesmo host; k6, LocalStack e Keycloak competem por CPU com o serviço. Os números são conservadores.
- O tráfego é só HTTP; o consumidor SQS é exercitado pelos testes de integração e de evidência, não aqui.
- O LocalStack não representa o throughput do SQS real; o atraso da outbox medido é um teto do ambiente, não do serviço.
