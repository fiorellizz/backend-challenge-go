# Evidências de execução distribuída

Gerado por `make evidence` em 2026-09-19T03:01:48Z contra três instâncias reais da aplicação (`app-1`, `app-2`, `app-3` do Docker Compose), cada uma com seu próprio processo, pool de conexões e memória, compartilhando PostgreSQL, LocalStack (SQS) e Keycloak. Os cenários abaixo correspondem ao item 13 do desafio.

Cada seção descreve o que foi feito, o que foi observado no banco, nas filas e na API, e o resultado. Os identificadores externos recebem um prefixo aleatório por execução, então o arquivo pode ser regenerado a qualquer momento.

## Mesma aposta enviada 50 vezes em paralelo por 3 instâncias

Uma carteira com 100.00; a mesma operação (mesma chave de idempotência) submetida 50 vezes simultaneamente, distribuída por `app-1`, `app-2` e `app-3`.

| Observação | Valor |
|---|---|
| Respostas 200 | 50 |
| Processamentos originais (`idempotentReplay=false`) | 1 |
| Replays (`idempotentReplay=true`) | 49 |
| Débitos no ledger | 1 |
| Saldo final | 90.00 |

**Resultado:** ✅ garantido

## Duas apostas de 80.00 disputando 100.00 em instâncias diferentes

`race-a` enviada para `app-2` e `race-b` para `app-3` ao mesmo tempo; depois as duas reenviadas.

| Observação | Valor |
|---|---|
| 1ª rodada: processadas / rejeitadas | 1 / 1 |
| Reenvio: processadas / rejeitadas | 1 / 1 |
| Código da rejeição | INSUFFICIENT_BALANCE |
| Débitos no ledger | 1 |
| Saldo final | 20.00 |

**Resultado:** ✅ garantido

## Carteiras distintas avançando em paralelo

12 carteiras × 10 apostas de 1.00, todas ao mesmo tempo, distribuídas pelas três instâncias.

| Observação | Valor |
|---|---|
| Operações | 120 |
| Erros | 0 |
| Tempo total | 371ms |
| Carteiras com saldo 90.00 e reconciliação consistente | 12 / 12 |

**Resultado:** ✅ garantido

## Consumidor morto (SIGKILL) durante o processamento de mensagens SQS

200 mensagens (5 carteiras × 40 apostas de 1.00) enviadas à fila FIFO; após 700ms, `docker compose kill -s SIGKILL app-2`. As mensagens que `app-2` tinha recebido e não removido voltaram à fila após o visibility timeout (30s) e foram tratadas por `app-1`/`app-3`; as já confirmadas no banco viraram replays pela inbox.

| Observação | Valor |
|---|---|
| Mensagens processadas / rejeitadas / pendentes | 200 / 0 / 0 |
| Linhas na inbox para essas mensagens | 200 |
| Débitos no ledger (esperado 200) | 200 |
| Carteiras com saldo 960.00 e reconciliação consistente | 5 / 5 |
| Tempo até todas concluírem após o kill | 1s |

**Resultado:** ✅ garantido

## Publisher da outbox morto (SIGKILL) no meio de um lote

150 operações commitadas geram 310 eventos na outbox (Processed + BalanceChanged por operação, mais os da abertura). Com os três publishers drenando, `docker compose kill -s SIGKILL app-3`. Linhas reivindicadas por `app-3` (FOR UPDATE SKIP LOCKED) foram liberadas pelo abort da transação e assumidas pelas outras instâncias; um evento publicado antes do commit da marcação é republicado com o mesmo `eventId` (deduplicado pela fila).

| Observação | Valor |
|---|---|
| Eventos gerados para as carteiras do cenário | 310 |
| Eventos ainda pendentes ao final | 0 |
| Maior número de tentativas registrado | 1 |
| Tempo até a outbox esvaziar após o kill | 1.049s |

**Resultado:** ✅ garantido

## Reinício da instância com uma reversão pendente

REFUND registrado como `PENDING_REFERENCE` em `app-1`; `docker compose restart app-1`; a BET referenciada chega por `app-2`; o worker de referências de qualquer instância retoma a pendência.

| Observação | Valor |
|---|---|
| Estado inicial do REFUND | PENDING_REFERENCE |
| BET durante o reinício | 200 PROCESSED |
| REFUND após o reinício | PROCESSED |
| Replay do REFUND em `app-1` reiniciada | 200 idempotentReplay=true |
| Saldo final / reconciliação | 100.00 / consistente=true |

**Resultado:** ✅ garantido

## PostgreSQL indisponível durante o tráfego

30 mensagens na fila e 10 requisições HTTP concorrentes com o PostgreSQL pausado (`docker compose pause postgres`) por 12s: as conexões TCP congelam sem erro, então o que protege o serviço é o timeout por requisição (10s → 503) e o backoff de reentrega do consumidor. Nada é aplicado duas vezes; HTTP responde 503 e o cliente reenvia.

| Observação | Valor |
|---|---|
| HTTP durante a pausa: 503 / 200 | 10 / 0 |
| Reenvios HTTP após a volta que resultaram em 200 | 10 / 10 |
| Mensagens SQS processadas / pendentes | 30 / 0 |
| Débitos no ledger (esperado 40) | 40 |
| Saldo final / reconciliação | 960.00 / consistente=true |

**Resultado:** ✅ garantido

## Parada graciosa (SIGTERM) de uma instância sob carga

60 mensagens na fila e 40 requisições HTTP concorrentes contra `app-2`; `docker compose stop app-2` (SIGTERM) 150ms depois. A instância para de aceitar conexões e de buscar mensagens, conclui o que está em curso e fecha o pool por último.

| Observação | Valor |
|---|---|
| HTTP contra a instância parando: 200 / recusadas | 40 / 0 |
| Tempo do `docker compose stop` | 894ms |
| Mensagens SQS processadas / pendentes | 60 / 0 |
| Débitos no ledger (esperado 100) | 100 |
| Reconciliação consistente | true |

**Resultado:** ✅ garantido

