package merge_with_label

import (
	"encoding/json"
	"time"

	"github.com/adjust/rmq/v5"
	"github.com/rs/zerolog"
)

type PushBackQueueConsumer struct {
	Queue         rmq.Queue
	PushBackQueue rmq.Queue
	Logger        *zerolog.Logger
}

func (h *PushBackQueueConsumer) Consume(delivery rmq.Delivery) {
	if err := delivery.Ack(); err != nil {
		h.Logger.Error().Err(err).Msg("unable to ack queue message")
		return
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(delivery.Payload()), &m); err != nil {
		h.Logger.Error().Err(err).Msg("unable to decode queue message into map")
		return
	}
	var hdr QueueMessage
	if err := decodeMap(m, &hdr, true); err != nil {
		h.Logger.Error().Err(err).Msg("unable to decode queue message")
		return
	}

	h.Logger.Debug().Str("id", hdr.ID).Int("type", int(hdr.Kind)).Msg("incoming message")

	if !hdr.DelayUntil.IsZero() && hdr.DelayUntil.After(time.Now()) {
		h.Logger.Debug().Msg("message not yet ready")
		time.Sleep(time.Second * 10)
		if err := h.PushBackQueue.Publish(delivery.Payload()); err != nil {
			h.Logger.Error().Err(err).Msg("unable to re-publish message")
			return
		}
		return
	}
	if err := h.Queue.Publish(delivery.Payload()); err != nil {
		h.Logger.Error().Err(err).Msg("unable to re-publish message")
		return
	}
}
