package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

// InboxRepository implements usecase.InboxRepository.
type InboxRepository struct {
	q querier
}

const (
	selectInbox = `
		SELECT payload_hash, received_at, completed_at
		  FROM inbox_messages
		 WHERE consumer_name = $1 AND message_id = $2`

	// Inserted in the same transaction as the domain changes, so the row
	// exists exactly when the handling of the message was committed.
	insertInbox = `
		INSERT INTO inbox_messages (id, consumer_name, message_id, payload_hash, received_at, completed_at)
		VALUES ($1, $2, $3, $4, $5, $6)`
)

func (r *InboxRepository) Find(ctx context.Context, consumerName, messageID string) (usecase.InboxMessage, bool, error) {
	msg := usecase.InboxMessage{ConsumerName: consumerName, MessageID: messageID}
	err := r.q.QueryRow(ctx, selectInbox, consumerName, messageID).Scan(&msg.PayloadHash, &msg.ReceivedAt, &msg.CompletedAt)
	if err != nil {
		if errors.Is(mapError(err), errs.ErrNotFound) {
			return usecase.InboxMessage{}, false, nil
		}
		return usecase.InboxMessage{}, false, fmt.Errorf("find inbox message: %w", mapError(err))
	}
	msg.ReceivedAt, msg.CompletedAt = msg.ReceivedAt.UTC(), msg.CompletedAt.UTC()
	return msg, true, nil
}

func (r *InboxRepository) Insert(ctx context.Context, msg usecase.InboxMessage) error {
	_, err := r.q.Exec(ctx, insertInbox,
		uuid.Must(uuid.NewV7()).String(), msg.ConsumerName, msg.MessageID, msg.PayloadHash,
		msg.ReceivedAt.UTC(), msg.CompletedAt.UTC())
	if err != nil {
		return fmt.Errorf("insert inbox message: %w", mapError(err))
	}
	return nil
}
