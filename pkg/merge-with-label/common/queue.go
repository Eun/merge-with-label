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
	return enqueue(ctx, logger, store, rateLimitInterval, MsgTypeRepo, dedupKey, msg)
}

// EnqueuePR enqueues a PR-level work item.
// All events targeting the same PR (pull_request, pull_request_review,
// check_run, push/status fan-out) share the same dedup key so only one row
// ever exists in the queue per PR at a time.
func EnqueuePR(
	ctx context.Context,
	logger *zerolog.Logger,
	store *pgqueue.Store,
	rateLimitInterval time.Duration,
	msg *QueuePRMessage,
) error {
	dedupKey := PRDedupKey(msg.Repository.NodeID, msg.PullRequest.Number)
	return enqueue(ctx, logger, store, rateLimitInterval, MsgTypePR, dedupKey, msg)
}

func enqueue(
	ctx context.Context,
	logger *zerolog.Logger,
	store *pgqueue.Store,
	rateLimitInterval time.Duration,
	msgType,
	dedupKey string,
	msg any,
) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return errors.Wrap(err, "unable to encode message")
	}

	availableAt := time.Now()
	if rateLimitInterval > 0 {
		lastBytes, err := store.KVGet(ctx, kvBucketRateLimit, dedupKey)
		if err != nil {
			return errors.Wrap(err, "unable to get rate limit from store")
		}
		if len(lastBytes) == 8 { //nolint:mnd // 8 bytes for int64
			lastUnix := int64(lastBytes[0]) | int64(lastBytes[1])<<8 | int64(lastBytes[2])<<16 | int64(lastBytes[3])<<24 | //nolint:mnd // bit shifts
				int64(lastBytes[4])<<32 | int64(lastBytes[5])<<40 | int64(lastBytes[6])<<48 | int64(lastBytes[7])<<56 //nolint:mnd // bit shifts
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
			return errors.Wrap(err, "unable to store rate limit in store")
		}
	}

	if err := store.Enqueue(ctx, msgType, dedupKey, payload, availableAt); err != nil {
		return errors.Wrap(err, "unable to enqueue message")
	}
	logger.Debug().Str("msg_type", msgType).Str("dedup_key", dedupKey).Msg("enqueued message")
	return nil
}
