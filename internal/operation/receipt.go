// Package operation provides transaction-scoped idempotency receipts. A caller
// holds its domain lock before reading or storing a receipt; retries return the
// original result, and reusing a key with different input is rejected.
package operation

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var ErrKeyReuse = errors.New("request_id was already used with different parameters")
var ErrKeyRequired = errors.New("request_id must contain 1–128 non-whitespace characters")

type Error struct {
	Code            string `json:"code"`
	Message         string `json:"message"`
	NextAction      string `json:"next_action"`
	CurrentRevision int64  `json:"current_revision,omitempty"`
}

func (e *Error) Error() string { return e.Message }

func ValidateKey(key string) error {
	if len(key) == 0 || len(key) > 128 || strings.TrimSpace(key) != key || strings.ContainsAny(key, "\r\n\x00") {
		return ErrKeyRequired
	}
	return nil
}

func fingerprint(input any) ([]byte, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(raw)
	return h[:], nil
}

func Replay(ctx context.Context, tx *sql.Tx, scope, key string, input, output any) (bool, error) {
	if err := ValidateKey(key); err != nil {
		return false, err
	}
	hash, err := fingerprint(input)
	if err != nil {
		return false, err
	}
	var matches bool
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT fingerprint=$3, result FROM operation_receipts WHERE scope=$1 AND request_id=$2`, scope, key, hash).Scan(&matches, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !matches {
		return false, ErrKeyReuse
	}
	if err = json.Unmarshal(raw, output); err != nil {
		return false, fmt.Errorf("decode receipt: %w", err)
	}
	return true, nil
}

func Save(ctx context.Context, tx *sql.Tx, scope, key string, input, output any) error {
	hash, err := fingerprint(input)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(output)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO operation_receipts(scope,request_id,fingerprint,result) VALUES($1,$2,$3,$4)`, scope, key, hash, string(raw))
	return err
}
