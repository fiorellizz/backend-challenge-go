SHELL := /bin/bash
.DEFAULT_GOAL := help

DB_URL      ?= postgres://wager:wager@localhost:5432/wager?sslmode=disable
MIGRATE     := docker run --rm --network host -v $(PWD)/migrations:/migrations migrate/migrate:v4.18.1
KC_URL      ?= http://localhost:8080
KC_REALM    ?= wager
API         ?= http://localhost:8081
AWSLOCAL    := docker compose exec -T localstack awslocal

.PHONY: help
help: ## Lista os alvos disponiveis
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------------------
# Infraestrutura
# ---------------------------------------------------------------------------

.PHONY: up
up: ## Sobe toda a stack (postgres, localstack, keycloak, 3 instancias)
	docker compose up --build -d
	@$(MAKE) --no-print-directory wait

.PHONY: wait
wait: ## Espera todos os servicos ficarem saudaveis
	@echo ">> aguardando servicos..."
	@for i in $$(seq 1 60); do \
		if docker compose ps --format '{{.Service}} {{.Health}}' \
			| grep -Ev '^(migrate)' | grep -q 'unhealthy\|starting'; then \
			sleep 3; \
		else \
			echo ">> stack pronta"; exit 0; \
		fi; \
	done; \
	echo "!! timeout aguardando servicos"; docker compose ps; exit 1

.PHONY: infra
infra: ## Sobe apenas as dependencias (sem as instancias da aplicacao)
	docker compose up -d postgres localstack keycloak migrate

.PHONY: down
down: ## Derruba a stack preservando volumes
	docker compose down

.PHONY: clean
clean: ## Derruba a stack e apaga volumes e orfaos
	docker compose down -v --remove-orphans

.PHONY: logs
logs: ## Segue os logs das instancias da aplicacao
	docker compose logs -f app-1 app-2 app-3

.PHONY: ps
ps: ## Estado dos containers
	docker compose ps

.PHONY: psql
psql: ## Abre um psql no banco
	docker compose exec postgres psql -U wager -d wager

.PHONY: queues
queues: ## Lista as filas SQS e seus atributos
	@$(AWSLOCAL) sqs list-queues
	@$(AWSLOCAL) sqs get-queue-attributes \
		--queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
		--attribute-names All

# ---------------------------------------------------------------------------
# Migrations
# ---------------------------------------------------------------------------

.PHONY: migrate-up
migrate-up: ## Aplica todas as migrations
	$(MIGRATE) -path=/migrations -database "$(DB_URL)" up

.PHONY: migrate-down
migrate-down: ## Reverte a ultima migration
	$(MIGRATE) -path=/migrations -database "$(DB_URL)" down 1

.PHONY: migrate-reset
migrate-reset: ## Reverte todas as migrations
	$(MIGRATE) -path=/migrations -database "$(DB_URL)" down -all

.PHONY: migrate-version
migrate-version: ## Mostra a versao atual do schema
	$(MIGRATE) -path=/migrations -database "$(DB_URL)" version

.PHONY: migrate-create
migrate-create: ## Cria um par de migrations (uso: make migrate-create name=add_x)
	@test -n "$(name)" || (echo "uso: make migrate-create name=add_x"; exit 1)
	$(MIGRATE) create -ext sql -dir /migrations -seq $(name)

# ---------------------------------------------------------------------------
# Build e qualidade
# ---------------------------------------------------------------------------

.PHONY: build
build: ## Compila o binario
	go build -o bin/app ./cmd/app

.PHONY: fmt
fmt: ## Formata o codigo
	gofmt -s -w .

.PHONY: fmt-check
fmt-check: ## Falha se houver arquivo nao formatado
	@out=$$(gofmt -s -l .); \
	if [ -n "$$out" ]; then echo "arquivos nao formatados:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## Executa go vet
	go vet ./...

.PHONY: tidy
tidy: ## Sincroniza go.mod e go.sum
	go mod tidy

# ---------------------------------------------------------------------------
# Testes
# ---------------------------------------------------------------------------

.PHONY: test
test: ## Testes unitarios (sem infraestrutura)
	go test ./... -count=1

.PHONY: race
race: ## Testes com detector de corrida
	go test -race ./... -count=1

.PHONY: test-integration
test-integration: ## Testes de integracao com containers reais
	go test -tags=integration ./test/integration/... -count=1 -timeout=15m -v

.PHONY: test-integration-race
test-integration-race: ## Integracao com detector de corrida
	go test -race -tags=integration ./test/integration/... -count=1 -timeout=20m

.PHONY: cover
cover: ## Relatorio de cobertura em coverage.html
	go test ./... -coverprofile=coverage.out -count=1
	go tool cover -html=coverage.out -o coverage.html
	@echo ">> coverage.html gerado"

.PHONY: check
check: fmt-check vet test race ## Portao de qualidade antes do commit

# ---------------------------------------------------------------------------
# Evidencias
# ---------------------------------------------------------------------------

.PHONY: evidence
evidence: up ## Roda os cenarios de falha e concorrencia e gera EVIDENCE.md
	go test -tags=evidence ./test/evidence/... -count=1 -timeout=20m -v
	@echo ">> EVIDENCE.md gerado na raiz do repositorio"

# ---------------------------------------------------------------------------
# Carga
# ---------------------------------------------------------------------------

.PHONY: load
load: ## Teste de carga com k6 (docker) contra as instancias do compose
	docker run --rm --network host -v $(PWD)/deploy/k6:/scripts:ro \
		-e API=$(API) -e KC_URL=$(KC_URL) grafana/k6:0.54.0 run /scripts/wager.js

# ---------------------------------------------------------------------------
# Auxiliares de desenvolvimento
# ---------------------------------------------------------------------------

.PHONY: token
token: ## Access token do provider-a (uso: make token client=provider-b)
	@curl -sS -X POST "$(KC_URL)/realms/$(KC_REALM)/protocol/openid-connect/token" \
		-H "Content-Type: application/x-www-form-urlencoded" \
		-d "grant_type=client_credentials" \
		-d "client_id=$(or $(client),provider-a)" \
		-d "client_secret=$(or $(secret),provider-a-secret)" \
		| python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])"

.PHONY: token-internal
token-internal: ## Access token do servico interno (abertura de carteira)
	@$(MAKE) --no-print-directory token client=wallet-internal secret=wallet-internal-secret

.PHONY: smoke
smoke: ## Abre uma carteira e envia uma aposta de 25.00 (checagem manual rapida)
	@set -euo pipefail; \
	INTERNAL=$$($(MAKE) --no-print-directory token-internal); \
	PROVIDER=$$($(MAKE) --no-print-directory token); \
	PLAYER=$$(python3 -c "import uuid; print(uuid.uuid4())"); \
	echo ">> abrindo carteira para player $$PLAYER"; \
	WALLET=$$(curl -sS -X POST "$(API)/wallets" \
		-H "Authorization: Bearer $$INTERNAL" \
		-H "Content-Type: application/json" \
		-d "{\"playerId\":\"$$PLAYER\",\"initialBalance\":{\"amount\":\"1000.00\",\"currency\":\"BRL\"}}"); \
	echo "$$WALLET"; \
	WALLET_ID=$$(echo "$$WALLET" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])"); \
	TX=tx-$$RANDOM; \
	echo ">> enviando BET de 25.00"; \
	curl -sS -X POST "$(API)/wagering/transactions" \
		-H "Authorization: Bearer $$PROVIDER" \
		-H "Content-Type: application/json" \
		-H "Idempotency-Key: provider-a:$$TX" \
		-d "{\"providerId\":\"provider-a\",\"externalTransactionId\":\"$$TX\",\"playerId\":\"$$PLAYER\",\"walletId\":\"$$WALLET_ID\",\"roundId\":\"round-1\",\"gameId\":\"fortune-chimp\",\"kind\":\"BET\",\"money\":{\"amount\":\"25.00\",\"currency\":\"BRL\"}}"; \
	echo; \
	echo ">> reconciliacao"; \
	curl -sS -X POST "$(API)/wallets/$$WALLET_ID/reconciliation" \
		-H "Authorization: Bearer $$INTERNAL"; \
	echo
