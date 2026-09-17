# Wager Service

Serviço em Go que processa operações financeiras de provedores de jogos
(`BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK`) sobre carteiras de jogadores, por
HTTP e por SQS, com as mesmas garantias nas duas entradas e em múltiplas
instâncias.

O enunciado completo do desafio está em [`docs/challenge.md`](docs/challenge.md).

## Stack

- Go 1.27, composição com [Uber Fx](https://github.com/uber-go/fx)
- PostgreSQL 16 (`pgx`, SQL explícito, migrations versionadas)
- AWS SQS via LocalStack (filas FIFO com DLQ)
- Keycloak como IdP OAuth 2.0 / OIDC (`client_credentials`)
- Docker Compose com três instâncias independentes da aplicação

## Comandos

```sh
make help   # lista todos os alvos
```

Instruções completas de execução, variáveis de ambiente, migrations e testes
serão documentadas aqui conforme o serviço evolui. As decisões de arquitetura
ficam em `ARCHITECTURE.md`.
