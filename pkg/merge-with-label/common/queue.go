package common

import (
	"crypto/md5" //nolint: gosec // allow weak cryptographic, md5 is just used for creating a unique kv key
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
)

func QueueMessage(
	logger *zerolog.Logger,
	js nats.JetStreamContext,
	kv nats.KeyValue,
	interval time.Duration,
	subject,
	msgID string,
	msg any,
) error {
	const bufSize = 8 // 64 bit
	//nolint: gosec // allow weak cryptographic, md5 is just used for creating a unique kv key
	h := md5.Sum([]byte(msgID))
	msgIDHash := hex.EncodeToString(h[:])
	entry, err := kv.Get(msgIDHash)
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		return errors.Wrap(err, "unable to get rate limit from kv bucket")
	}
	if errors.Is(err, nats.ErrKeyNotFound) {
		entry = nil
	}
	var lastMessageSendTime time.Time
	if entry != nil && len(entry.Value()) != 0 {
		lastMessageSendTime = time.Unix(int64(binary.LittleEndian.Uint64(entry.Value())), 0)
	}

	header := make(nats.Header)
	diff := time.Until(lastMessageSendTime.Add(interval))
	if diff > 0 {
		// the same message was already sent in the interval
		// add a msg id to skip duplicate message
		// and add a header to delay the message until the interval was hit
		header.Set(nats.MsgIdHdr, msgIDHash)
		header.Set(DelayUntilHeader, time.Now().Add(diff).Format(time.RFC3339))
	}
	buf, err := json.Marshal(msg)
	if err != nil {
		return errors.Wrap(err, "unable to encode message")
	}

	_, err = js.PublishMsgAsync(&nats.Msg{
		Subject: subject,
		Header:  header,
		Data:    buf,
	})

	if err != nil {
		return errors.Wrap(err, "unable to publish message to queue")
	}
	logger.
		Debug().
		Msg("published message")

	b := make([]byte, bufSize)
	binary.LittleEndian.PutUint64(b, uint64(time.Now().UTC().Unix()))
	_, err = kv.Put(msgIDHash, b)
	if err != nil {
		return errors.Wrap(err, "unable to store last message time in kv bucket")
	}
	return nil
}

func DelayMessageIfNeeded(logger *zerolog.Logger, msg *nats.Msg) bool {
	delayUntilValue := msg.Header.Get(DelayUntilHeader)
	if delayUntilValue == "" {
		return false
	}
	delayUntil, err := time.Parse(time.RFC3339, delayUntilValue)
	if err != nil {
		logger.Error().Err(err).Msg("unable to parse delay until header")
		if err := msg.Nak(); err != nil {
			logger.Error().Err(err).Msg("unable to nak message")
		}
		return true
	}
	diff := time.Until(delayUntil)
	if diff > 0 {
		logger.Debug().Str("id", msg.Header.Get(nats.MsgIdHdr)).Msg("message not yet ready")
		if err := msg.NakWithDelay(diff); err != nil {
			logger.Error().Err(err).Msg("unable to nak delay message")
		}
		return true
	}
	return false
}
