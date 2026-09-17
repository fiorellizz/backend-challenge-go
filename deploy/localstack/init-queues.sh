#!/bin/bash
# Provisiona as filas SQS assim que o LocalStack fica pronto.
#
#   wager-transactions.fifo       entrada de operacoes (WagerTransactionRequested)
#   wager-transactions-dlq.fifo   destino apos tentativas esgotadas
#   wager-events.fifo             saida dos eventos de integracao (outbox)
#
# FIFO com ContentBasedDeduplication=false: o produtor informa explicitamente o
# MessageDeduplicationId. A deduplicacao nativa do SQS (janela de 5 minutos) e
# apenas uma conveniencia; a garantia real de idempotencia vive no PostgreSQL.

set -euo pipefail

REGION="${AWS_DEFAULT_REGION:-us-east-1}"
ACCOUNT_ID="000000000000"

echo "[init-queues] criando filas FIFO na regiao ${REGION}"

# --- DLQ ---------------------------------------------------------------------
awslocal sqs create-queue \
  --queue-name wager-transactions-dlq.fifo \
  --attributes '{
    "FifoQueue": "true",
    "ContentBasedDeduplication": "false",
    "MessageRetentionPeriod": "1209600"
  }' >/dev/null

DLQ_ARN="arn:aws:sqs:${REGION}:${ACCOUNT_ID}:wager-transactions-dlq.fifo"

# --- Fila principal ----------------------------------------------------------
# maxReceiveCount=5: apos cinco entregas sem remocao bem-sucedida, a mensagem
# vai para a DLQ. VisibilityTimeout=30s cobre a transacao de dominio com folga.
awslocal sqs create-queue \
  --queue-name wager-transactions.fifo \
  --attributes "{
    \"FifoQueue\": \"true\",
    \"ContentBasedDeduplication\": \"false\",
    \"VisibilityTimeout\": \"30\",
    \"ReceiveMessageWaitTimeSeconds\": \"10\",
    \"MessageRetentionPeriod\": \"1209600\",
    \"RedrivePolicy\": \"{\\\"deadLetterTargetArn\\\":\\\"${DLQ_ARN}\\\",\\\"maxReceiveCount\\\":\\\"5\\\"}\"
  }" >/dev/null

# --- Saida de eventos de integracao -----------------------------------------
awslocal sqs create-queue \
  --queue-name wager-events.fifo \
  --attributes '{
    "FifoQueue": "true",
    "ContentBasedDeduplication": "false",
    "VisibilityTimeout": "30",
    "MessageRetentionPeriod": "1209600"
  }' >/dev/null

echo "[init-queues] filas provisionadas:"
awslocal sqs list-queues
