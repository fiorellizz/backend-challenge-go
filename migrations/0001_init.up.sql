-- 0001_init.up.sql
-- Schema base: carteiras, transacoes, ledger append-only, inbox e outbox.
--
-- Principio: as invariantes financeiras sao impostas PELO BANCO. A camada de
-- dominio tambem as valida, mas o banco e a ultima linha de defesa e continua
-- valendo sob concorrencia, retry, bug de aplicacao ou acesso manual.

BEGIN;

-- ---------------------------------------------------------------------------
-- Tipos
-- ---------------------------------------------------------------------------

CREATE TYPE wager_transaction_kind AS ENUM (
    'OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK'
);

CREATE TYPE wager_transaction_status AS ENUM (
    'PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED'
);

CREATE TYPE wager_transaction_origin AS ENUM ('INTERNAL', 'EXTERNAL');

CREATE TYPE ledger_direction AS ENUM ('DEBIT', 'CREDIT');


-- ---------------------------------------------------------------------------
-- wallets
-- ---------------------------------------------------------------------------
-- Raiz do agregado financeiro. O saldo so muda sob lock da propria linha
-- (SELECT ... FOR UPDATE), o que serializa escritores da MESMA carteira sem
-- afetar carteiras distintas.

CREATE TABLE wallets (
    id              UUID        PRIMARY KEY,
    player_id       UUID        NOT NULL,
    currency        CHAR(3)     NOT NULL,
    balance_minor   BIGINT      NOT NULL,
    version         INTEGER     NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Invariante central: saldo nunca negativo. Se a regra de dominio falhar,
    -- a transacao aborta aqui.
    CONSTRAINT wallets_balance_non_negative CHECK (balance_minor >= 0),
    CONSTRAINT wallets_version_positive     CHECK (version >= 1),
    CONSTRAINT wallets_currency_iso4217     CHECK (currency ~ '^[A-Z]{3}$'),

    -- (playerId, currency) identifica uma unica carteira.
    CONSTRAINT wallets_player_currency_unique UNIQUE (player_id, currency)
);


-- ---------------------------------------------------------------------------
-- wager_transactions
-- ---------------------------------------------------------------------------
-- Guarda operacoes externas (provedor) e internas (abertura de carteira).
-- Colunas de metadados externos sao NULL na origem interna, e os CHECKs abaixo
-- impedem qualquer mistura entre as duas formas.

CREATE TABLE wager_transactions (
    id                                UUID        PRIMARY KEY,
    origin                            wager_transaction_origin  NOT NULL,

    -- Metadados externos (NULL quando origin = 'INTERNAL')
    provider_id                       TEXT,
    external_transaction_id           TEXT,
    idempotency_key                   TEXT,
    payload_hash                      BYTEA,
    round_id                          TEXT,
    game_id                           TEXT,

    -- Alvo da operacao
    wallet_id                         UUID        NOT NULL REFERENCES wallets(id),
    player_id                         UUID        NOT NULL,
    kind                              wager_transaction_kind    NOT NULL,
    amount_minor                      BIGINT      NOT NULL,
    currency                          CHAR(3)     NOT NULL,

    -- Referencia de reversao (REFUND / ROLLBACK)
    reference_external_transaction_id TEXT,
    reference_transaction_id          UUID        REFERENCES wager_transactions(id),

    -- Estado e resultado
    status                            wager_transaction_status  NOT NULL,
    failure_code                      TEXT,
    balance_after_minor               BIGINT,
    wallet_version_after              INTEGER,

    -- Retomada de referencia pendente
    reference_attempts                INTEGER     NOT NULL DEFAULT 0,
    next_reference_attempt_at         TIMESTAMPTZ,
    reference_deadline_at             TIMESTAMPTZ,

    correlation_id                    TEXT,
    created_at                        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                        TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at                      TIMESTAMPTZ,

    CONSTRAINT wager_tx_currency_iso4217 CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT wager_tx_amount_non_negative CHECK (amount_minor >= 0),

    -- Politica de valor por tipo.
    CONSTRAINT wager_tx_loss_is_zero CHECK (
        kind <> 'LOSS' OR amount_minor = 0
    ),
    CONSTRAINT wager_tx_movement_is_positive CHECK (
        kind NOT IN ('OPENING', 'BET', 'WIN', 'REFUND', 'ROLLBACK')
        OR amount_minor > 0
    ),

    -- Coerencia entre origem interna e externa. Um dos dois formatos, nunca
    -- um hibrido.
    CONSTRAINT wager_tx_origin_shape CHECK (
        (
            origin = 'EXTERNAL'
            AND kind <> 'OPENING'
            AND provider_id             IS NOT NULL
            AND external_transaction_id IS NOT NULL
            AND idempotency_key         IS NOT NULL
            AND payload_hash            IS NOT NULL
            AND round_id                IS NOT NULL
            AND game_id                 IS NOT NULL
        )
        OR
        (
            origin = 'INTERNAL'
            AND kind = 'OPENING'
            AND provider_id                       IS NULL
            AND external_transaction_id           IS NULL
            AND idempotency_key                   IS NULL
            AND payload_hash                      IS NULL
            AND round_id                          IS NULL
            AND game_id                           IS NULL
            AND reference_external_transaction_id IS NULL
            AND reference_transaction_id          IS NULL
        )
    ),

    -- Reversao exige referencia externa informada.
    CONSTRAINT wager_tx_reversal_has_reference CHECK (
        kind NOT IN ('REFUND', 'ROLLBACK')
        OR reference_external_transaction_id IS NOT NULL
    ),

    -- Toda rejeicao carrega um codigo estavel.
    CONSTRAINT wager_tx_rejected_has_failure_code CHECK (
        status <> 'REJECTED' OR failure_code IS NOT NULL
    ),

    -- Resultado financeiro so existe em transacao concluida.
    CONSTRAINT wager_tx_processed_has_result CHECK (
        status <> 'PROCESSED'
        OR (balance_after_minor IS NOT NULL AND processed_at IS NOT NULL)
    ),

    -- Espera por referencia so faz sentido em reversao.
    CONSTRAINT wager_tx_pending_reference_is_reversal CHECK (
        status <> 'PENDING_REFERENCE' OR kind IN ('REFUND', 'ROLLBACK')
    )
);

-- Idempotencia camada 1: a chave recebida do cliente e unica.
CREATE UNIQUE INDEX wager_tx_idempotency_key_unique
    ON wager_transactions (idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- Idempotencia camada 2: a MESMA operacao financeira nao pode ser reaplicada
-- sob outra chave de idempotencia.
CREATE UNIQUE INDEX wager_tx_provider_external_unique
    ON wager_transactions (provider_id, external_transaction_id)
    WHERE provider_id IS NOT NULL;

-- Uma unica abertura por carteira: impede credito inicial duplicado.
CREATE UNIQUE INDEX wager_tx_opening_unique_per_wallet
    ON wager_transactions (wallet_id)
    WHERE kind = 'OPENING';

-- Uma transacao referenciada aceita no maximo UMA reversao bem-sucedida, seja
-- REFUND ou ROLLBACK. Mais restritivo que "uma de cada tipo", de proposito:
-- um REFUND seguido de um ROLLBACK sobre a mesma BET devolveria o mesmo debito
-- duas vezes. Rejeicoes e falhas nao ocupam o slot, entao um REFUND recusado
-- nao bloqueia um ROLLBACK posterior legitimo.
CREATE UNIQUE INDEX wager_tx_single_successful_reversal
    ON wager_transactions (reference_transaction_id)
    WHERE reference_transaction_id IS NOT NULL
      AND kind IN ('REFUND', 'ROLLBACK')
      AND status = 'PROCESSED';

-- Retomada durable de PENDING apos queda de instancia.
CREATE INDEX wager_tx_pending_resume_idx
    ON wager_transactions (created_at)
    WHERE status = 'PENDING';

-- Fila do worker de referencias pendentes.
CREATE INDEX wager_tx_pending_reference_idx
    ON wager_transactions (next_reference_attempt_at)
    WHERE status = 'PENDING_REFERENCE';

-- Resolucao de referencia por rodada e consultas por carteira.
CREATE INDEX wager_tx_wallet_created_idx
    ON wager_transactions (wallet_id, created_at DESC);


-- ---------------------------------------------------------------------------
-- wallet_ledger_entries
-- ---------------------------------------------------------------------------
-- Append-only. Fonte da verdade contabil: o saldo da wallet deve sempre ser
-- reconstrutivel como soma(CREDIT) - soma(DEBIT).

CREATE TABLE wallet_ledger_entries (
    id                   UUID        PRIMARY KEY,
    seq                  BIGSERIAL   NOT NULL,
    wallet_id            UUID        NOT NULL REFERENCES wallets(id),
    transaction_id       UUID        NOT NULL REFERENCES wager_transactions(id),
    direction            ledger_direction NOT NULL,
    amount_minor         BIGINT      NOT NULL,
    currency             CHAR(3)     NOT NULL,
    balance_before_minor BIGINT      NOT NULL,
    balance_after_minor  BIGINT      NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ledger_amount_positive CHECK (amount_minor > 0),
    CONSTRAINT ledger_currency_iso4217 CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT ledger_balances_non_negative CHECK (
        balance_before_minor >= 0 AND balance_after_minor >= 0
    ),

    -- balanceAfter = balanceBefore +/- amount, conforme a direcao.
    CONSTRAINT ledger_balance_arithmetic CHECK (
        (direction = 'CREDIT' AND balance_after_minor = balance_before_minor + amount_minor)
        OR
        (direction = 'DEBIT'  AND balance_after_minor = balance_before_minor - amount_minor)
    ),

    -- Idempotencia camada 3 e rede de seguranca final: uma transacao produz no
    -- maximo um lancamento por carteira. Mesmo que toda a logica acima falhe,
    -- movimentacao duplicada e impossivel.
    CONSTRAINT ledger_wallet_transaction_unique UNIQUE (wallet_id, transaction_id)
);

-- Paginacao por cursor. Entradas da mesma carteira sao escritas sob o lock da
-- linha da wallet, entao a ordem de seq por carteira e estavel: nao existe
-- intercalacao de commits concorrentes dentro de uma mesma carteira.
CREATE INDEX ledger_wallet_seq_idx ON wallet_ledger_entries (wallet_id, seq);


-- Imutabilidade imposta pelo banco, nao apenas por convencao de codigo.
CREATE OR REPLACE FUNCTION forbid_ledger_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION
        'wallet_ledger_entries is append-only; % is not allowed', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER wallet_ledger_entries_no_update
    BEFORE UPDATE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION forbid_ledger_mutation();

CREATE TRIGGER wallet_ledger_entries_no_delete
    BEFORE DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION forbid_ledger_mutation();

CREATE TRIGGER wallet_ledger_entries_no_truncate
    BEFORE TRUNCATE ON wallet_ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_ledger_mutation();


-- ---------------------------------------------------------------------------
-- inbox_messages
-- ---------------------------------------------------------------------------
-- Deduplicacao de entrada por consumidor. Gravada na MESMA transacao das
-- mudancas de dominio: se o processo morre antes do commit, nada aconteceu;
-- se morre depois, a reentrega encontra o registro e nao reaplica.

CREATE TABLE inbox_messages (
    id            UUID        PRIMARY KEY,
    consumer_name TEXT        NOT NULL,
    message_id    TEXT        NOT NULL,
    payload_hash  BYTEA       NOT NULL,
    received_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at  TIMESTAMPTZ,

    CONSTRAINT inbox_consumer_message_unique UNIQUE (consumer_name, message_id)
);


-- ---------------------------------------------------------------------------
-- outbox_events
-- ---------------------------------------------------------------------------
-- Gravada na mesma transacao do fato que a originou. Nenhum evento sai antes
-- do commit. O worker publica com FOR UPDATE SKIP LOCKED: varios publishers
-- disputam sem conflito e trabalho abandonado por queda de instancia e
-- liberado pelo proprio abort da transacao, sem reaper.

CREATE TABLE outbox_events (
    event_id        UUID        PRIMARY KEY,
    aggregate_type  TEXT        NOT NULL,
    aggregate_id    UUID        NOT NULL,
    event_type      TEXT        NOT NULL,
    event_version   INTEGER     NOT NULL,
    correlation_id  TEXT        NOT NULL,
    causation_id    TEXT,
    payload         JSONB       NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL,
    attempts        INTEGER     NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at    TIMESTAMPTZ,
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT outbox_attempts_non_negative CHECK (attempts >= 0),
    CONSTRAINT outbox_version_positive      CHECK (event_version >= 1)
);

-- Fila de publicacao: indice parcial so sobre o que ainda nao foi publicado.
CREATE INDEX outbox_pending_idx
    ON outbox_events (next_attempt_at, event_id)
    WHERE published_at IS NULL;

-- Metrica de atraso da outbox.
CREATE INDEX outbox_occurred_at_idx
    ON outbox_events (occurred_at)
    WHERE published_at IS NULL;

COMMIT;
