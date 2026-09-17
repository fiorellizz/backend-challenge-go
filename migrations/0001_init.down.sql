-- 0001_init.down.sql
-- Reverte integralmente 0001_init.up.sql.

BEGIN;

DROP TRIGGER IF EXISTS wallet_ledger_entries_no_truncate ON wallet_ledger_entries;
DROP TRIGGER IF EXISTS wallet_ledger_entries_no_delete   ON wallet_ledger_entries;
DROP TRIGGER IF EXISTS wallet_ledger_entries_no_update   ON wallet_ledger_entries;

DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS inbox_messages;
DROP TABLE IF EXISTS wallet_ledger_entries;
DROP TABLE IF EXISTS wager_transactions;
DROP TABLE IF EXISTS wallets;

DROP FUNCTION IF EXISTS forbid_ledger_mutation();

DROP TYPE IF EXISTS ledger_direction;
DROP TYPE IF EXISTS wager_transaction_origin;
DROP TYPE IF EXISTS wager_transaction_status;
DROP TYPE IF EXISTS wager_transaction_kind;

COMMIT;
