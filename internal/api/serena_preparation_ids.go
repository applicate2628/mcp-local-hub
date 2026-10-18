package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
)

type SerenaPreparationIDs struct{ OperationID, GenerationID string }

func serenaRoleID(ctx context.Context, prefix string, read func([]byte) (int, error)) (string, error) {
	if ctx == nil || read == nil {
		return "", errors.New("SERENA_ID_CONTEXT_MISSING")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var nonce [16]byte
	n, err := read(nonce[:])
	if err != nil {
		return "", errors.Join(err, ctx.Err())
	}
	if n != len(nonce) {
		return "", errors.Join(errors.New("SERENA_ID_ENTROPY_PARTIAL"), ctx.Err())
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(nonce[:]), nil
}
func serenaPreparationIDs(ctx context.Context, read func([]byte) (int, error)) (SerenaPreparationIDs, error) {
	op, err := serenaRoleID(ctx, "operation-", read)
	if err != nil {
		return SerenaPreparationIDs{}, err
	}
	gen, err := serenaRoleID(ctx, "generation-", read)
	if err != nil {
		return SerenaPreparationIDs{}, err
	}
	return SerenaPreparationIDs{op, gen}, nil
}
func NewSerenaPreparationIDs(ctx context.Context) (SerenaPreparationIDs, error) {
	return serenaPreparationIDs(ctx, rand.Read)
}
func NewSerenaConsumerOperationID(ctx context.Context) (string, error) {
	return serenaRoleID(ctx, "consumer-", rand.Read)
}
func newSerenaPublisherOwnerInstanceID(ctx context.Context) (string, error) {
	return serenaRoleID(ctx, "", rand.Read)
}
