package wagering

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
)

// Payload is the set of business fields that identify an external
// operation. Transport metadata (idempotency key, message id, correlation
// id, headers) is deliberately not part of it.
type Payload struct {
	ProviderID                     string
	ExternalTransactionID          string
	PlayerID                       string
	WalletID                       string
	RoundID                        string
	GameID                         string
	Kind                           Kind
	Money                          money.Money
	ReferenceExternalTransactionID string
}

// Hash returns the SHA-256 of the canonical JSON form of the payload:
// object keys sorted, no whitespace, amount normalized to two decimals,
// reference present only when set. HTTP and SQS inputs that describe the
// same operation therefore produce the same hash, and a replay with any
// business field changed is detected as a conflict.
func (p Payload) Hash() ([]byte, error) {
	if err := p.Money.Validate(); err != nil {
		return nil, err
	}
	canonical := map[string]any{
		"providerId":            p.ProviderID,
		"externalTransactionId": p.ExternalTransactionID,
		"playerId":              p.PlayerID,
		"walletId":              p.WalletID,
		"roundId":               p.RoundID,
		"gameId":                p.GameID,
		"kind":                  string(p.Kind),
		"money": map[string]string{
			"amount":   p.Money.Amount(),
			"currency": p.Money.Currency().String(),
		},
	}
	if p.ReferenceExternalTransactionID != "" {
		canonical["referenceExternalTransactionId"] = p.ReferenceExternalTransactionID
	}
	// encoding/json sorts map keys and emits no whitespace.
	data, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("canonical payload: %w", err)
	}
	sum := sha256.Sum256(data)
	return sum[:], nil
}
