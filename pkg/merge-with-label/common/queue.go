package common

import (
	"context"
	"encoding/json"
	"time"

	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/pgqueue"
)

const kvBucketRateLimit = "rate_limit"

// EnqueueRepo enqueues a repo-level work item (push, status, etc.).
// All events for the same repo share the same dedup key so only one row
// ever exists in the queue per repo at a time.
func EnqueueRepo(
	ctx context.Context,
	logger *zerolog.Logger,
	store *pgqueue.Store,
	rateLimitInterval time.Duration,
	msg *QueueRepoMessage,
) error {
	dedupKey := RepoDedupKey(msg.Repository.NodeID)
	payload, availableAt, err := prepareEnqueue(ctx, store, rateLimitInterval, dedupKey, msg)
	if err != nil {
		return err
	}
	if err := store.EnqueueRepo(ctx, dedupKey, payload, availableAt); err != nil {
		return errors.Wrap(err, "unable to enqueue repo message")
	}
	logger.Debug().Str("dedup_key", dedupKey).Msg("enqueued repo message")
	return nil
}

// EnqueuePR enqueues a PR-level work item.
// All events targeting the same PR share the same dedup key so only one row
// ever exists in the queue per PR at a time.
func EnqueuePR(
	ctx context.Context,
	logger *zerolog.Logger,
	store *pgqueue.Store,
	rateLimitInterval time.Duration,
	msg *QueuePRMessage,
) error {
	dedupKey := PRDedupKey(msg.Repository.NodeID, msg.PullRequest.Number)
	payload, availableAt, err := prepareEnqueue(ctx, store, rateLimitInterval, dedupKey, msg)
	if err != nil {
		return err
	}
	if err := store.EnqueuePR(ctx, dedupKey, payload, availableAt); err != nil {
		return errors.Wrap(err, "unable to enqueue PR message")
	}
	logger.Debug().Str("dedup_key", dedupKey).Msg("enqueued PR message")
	return nil
}

// prepareEnqueue serialises msg and computes the rate-limit delay.
func prepareEnqueue(
	ctx context.Context,
	store *pgqueue.Store,
	rateLimitInterval time.Duration,
	dedupKey string,
	msg any,
) (payload []byte, availableAt time.Time, err error) {
	payload, err = json.Marshal(msg)
	if err != nil {
		return nil, time.Time{}, errors.Wrap(err, "unable to encode message")
	}

	availableAt = time.Now()
	if rateLimitInterval <= 0 {
		return payload, availableAt, nil
	}

	lastBytes, err := store.KVGet(ctx, kvBucketRateLimit, dedupKey)
	if err != nil {
		return nil, time.Time{}, errors.Wrap(err, "unable to get rate limit from store")
	}
	if len(lastBytes) == 8 { //nolint:mnd // 8 bytes for int64
		var lastUnix int64
		for i := range 8 {
			lastUnix |= int64(lastBytes[i]) << (i * 8) //nolint:mnd // bit shift per byte
		}
		if diff := time.Until(time.Unix(lastUnix, 0).Add(rateLimitInterval)); diff > 0 {
			availableAt = time.Now().Add(diff)
		}
	}

	now := time.Now().UTC().Unix()
	b := make([]byte, 8) //nolint:mnd // 8 bytes for int64
	for i := range 8 {
		b[i] = byte(now >> (i * 8)) //nolint:mnd // bit shift per byte
	}
	if err := store.KVSet(ctx, kvBucketRateLimit, dedupKey, b, rateLimitInterval*2); err != nil {
		return nil, time.Time{}, errors.Wrap(err, "unable to store rate limit in store")
	}

	return payload, availableAt, nil
}
