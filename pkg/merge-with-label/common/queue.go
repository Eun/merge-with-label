package common

import (
	"context"
	"encoding/json"
	"time"

	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/pgqueue"
)

// Queuer is the interface the server uses to enqueue messages.
type Queuer interface {
	Enqueue(ctx context.Context, msgType, dedupKey string, payload []byte, availableAt time.Time) error
}

// QueueMessage serialises msg to JSON and enqueues it.
// dedupKey identifies the logical "work item" — a second call with the same
// dedupKey within the same msgType is silently dropped (ON CONFLICT DO NOTHING).
// When rateLimitInterval > 0, a job that was already enqueued within that
// window is delayed until the window expires so rapid consecutive GitHub events
// for the same repo/PR collapse into a single deferred execution.
func QueueMessage(
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

	// Rate-limit: check when the last job for this dedupKey was enqueued.
	availableAt := time.Now()
	if rateLimitInterval > 0 {
		const kvBucket = "rate_limit"
		lastBytes, err := store.KVGet(ctx, kvBucket, dedupKey)
		if err != nil {
			return errors.Wrap(err, "unable to get rate limit from store")
		}
		if len(lastBytes) == 8 {
			var lastUnix int64
			lastUnix = int64(lastBytes[0]) | int64(lastBytes[1])<<8 | int64(lastBytes[2])<<16 | int64(lastBytes[3])<<24 |
				int64(lastBytes[4])<<32 | int64(lastBytes[5])<<40 | int64(lastBytes[6])<<48 | int64(lastBytes[7])<<56
			lastTime := time.Unix(lastUnix, 0)
			if diff := time.Until(lastTime.Add(rateLimitInterval)); diff > 0 {
				availableAt = time.Now().Add(diff)
			}
		}
		// Record now as the last enqueue time.
		now := time.Now().UTC().Unix()
		b := make([]byte, 8) //nolint:mnd // 8 bytes for int64
		b[0] = byte(now)
		b[1] = byte(now >> 8)  //nolint:mnd // bit shift
		b[2] = byte(now >> 16) //nolint:mnd // bit shift
		b[3] = byte(now >> 24) //nolint:mnd // bit shift
		b[4] = byte(now >> 32) //nolint:mnd // bit shift
		b[5] = byte(now >> 40) //nolint:mnd // bit shift
		b[6] = byte(now >> 48) //nolint:mnd // bit shift
		b[7] = byte(now >> 56) //nolint:mnd // bit shift
		if err := store.KVSet(ctx, kvBucket, dedupKey, b, rateLimitInterval*2); err != nil {
			return errors.Wrap(err, "unable to store rate limit in store")
		}
	}

	if err := store.Enqueue(ctx, msgType, dedupKey, payload, availableAt); err != nil {
		return errors.Wrap(err, "unable to enqueue message")
	}
	logger.Debug().Str("msg_type", msgType).Str("dedup_key", dedupKey).Msg("enqueued message")
	return nil
}
