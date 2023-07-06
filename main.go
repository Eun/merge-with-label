package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"time"

	merge_with_label "github.com/Eun/merge-with-label/pkg/merge-with-label"
	"github.com/adjust/rmq/v5"
	"github.com/pkg/errors"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	queueName         = "queue"
	pushBackQueueName = "queue-back"
	prefetchLimit     = 10
	pollDuration      = time.Second
	numConsumers      = 5
	maxRetries        = 5
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	address := os.Getenv("ADDRESS")
	if address == "" {
		address = ":" + os.Getenv("PORT")
	}

	if address == ":" {
		address = ":8000"
	}

	if os.Getenv("APP_ID") == "" {
		log.Error().Msg("APP_ID is not set")
		return
	}

	appID, err := strconv.ParseInt(os.Getenv("APP_ID"), 10, 64)
	if err != nil {
		log.Error().Err(err).Msg("unable to get APP_ID")
		return
	}

	privateKeyFile := os.Getenv("PRIVATE_KEY")
	if privateKeyFile == "" {
		log.Error().Msg("PRIVATE_KEY is not set")
		return
	}
	privateKeyBytes, err := os.ReadFile(privateKeyFile)
	if err != nil {
		log.Error().Str("file", privateKeyFile).Msg("unable to read private key")
		return
	}

	redisCacheClient := redis.NewClient(&redis.Options{
		Network:  "tcp",
		Addr:     os.Getenv("REDIS_HOST"),
		Username: os.Getenv("REDIS_USER"),
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       0,
	})
	defer redisCacheClient.Close()

	redisQueueClient := redis.NewClient(&redis.Options{
		Network:  "tcp",
		Addr:     os.Getenv("REDIS_HOST"),
		Username: os.Getenv("REDIS_USER"),
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       1,
	})
	defer redisQueueClient.Close()

	errChan := make(chan error, 10)

	producerConnection, err := rmq.OpenConnectionWithRedisClient("producer", redisQueueClient, errChan)
	if err != nil {
		log.Error().Msg("unable to open queue connection to redis")
		return
	}

	producerQueue, err := producerConnection.OpenQueue(queueName)
	if err != nil {
		log.Error().Err(err).Msg("unable to open queue")
		return
	}

	consumerConnection, err := rmq.OpenConnectionWithRedisClient("consumer", redisQueueClient, errChan)
	if err != nil {
		log.Error().Msg("unable to open queue connection to redis")
		return
	}

	producerPushBackConnection, err := rmq.OpenConnectionWithRedisClient("producer", redisQueueClient, errChan)
	if err != nil {
		log.Error().Msg("unable to open queue connection to redis")
		return
	}

	producerPushBackQueue, err := producerPushBackConnection.OpenQueue(pushBackQueueName)
	if err != nil {
		log.Error().Err(err).Msg("unable to open queue")
		return
	}

	consumerPushBackConnection, err := rmq.OpenConnectionWithRedisClient("consumer", redisQueueClient, errChan)
	if err != nil {
		log.Error().Msg("unable to open queue connection to redis")
		return
	}

	server := http.Server{
		Addr:              address,
		ReadTimeout:       1 * time.Second,
		WriteTimeout:      1 * time.Second,
		IdleTimeout:       30 * time.Second,
		ReadHeaderTimeout: 2 * time.Second,
		Handler: &merge_with_label.Handler{
			Queue: producerQueue,
			GetLoggerForContext: func(ctx context.Context) *zerolog.Logger {
				return &log.Logger
			},
			HTTPClient: http.DefaultClient,
			AppID:      appID,
			PrivateKey: privateKeyBytes,
		},
		BaseContext: func(listener net.Listener) context.Context {
			return ctx
		},
	}

	queue, err := consumerConnection.OpenQueue(queueName)
	if err != nil {
		log.Error().Err(err).Msg("unable to open queue")
		return
	}

	if err := queue.StartConsuming(prefetchLimit, pollDuration); err != nil {
		log.Error().Err(err).Msg("unable to start consuming")
		return
	}

	pushBackQueue, err := consumerPushBackConnection.OpenQueue(pushBackQueueName)
	if err != nil {
		log.Error().Err(err).Msg("unable to open queue")
		return
	}

	if err := pushBackQueue.StartConsuming(prefetchLimit, pollDuration); err != nil {
		log.Error().Err(err).Msg("unable to start consuming")
		return
	}

	go func() {
		if err := server.ListenAndServe(); err != nil {
			errChan <- errors.Wrapf(err, "unable to listen on address %s", address)
			return
		}
	}()

	go func() {
		cleanerConnection, err := rmq.OpenConnectionWithRedisClient("cleaner", redisQueueClient, errChan)
		if err != nil {
			log.Error().Msg("unable to open queue connection to redis")
			return
		}
		cleaner := rmq.NewCleaner(cleanerConnection)
		for {
			select {
			case <-time.After(time.Minute):
				returned, err := cleaner.Clean()
				if err != nil {
					log.Error().Err(err).Msg("unable to clean queue")
					continue
				}
				log.Debug().Msgf("cleaned %d", returned)
			case <-ctx.Done():
				return
			}
		}
	}()

	for i := 0; i < numConsumers; i++ {
		logger := log.Logger.With().Int("consumer", i).Logger()
		if _, err := queue.AddConsumer(
			fmt.Sprintf("consumer %d", i),
			&merge_with_label.QueueConsumer{
				PushBackQueue:    pushBackQueue,
				Logger:           &logger,
				HTTPClient:       http.DefaultClient,
				AppID:            appID,
				PrivateKey:       privateKeyBytes,
				RedisCacheClient: redisCacheClient,
				MaxRetries:       maxRetries,
			}); err != nil {
			log.Error().Err(err).Msg("unable to add consumer")
			return
		}
	}

	for i := 0; i < numConsumers; i++ {
		logger := log.Logger.With().Int("push_back_consumer", i).Logger()
		if _, err := pushBackQueue.AddConsumer(
			fmt.Sprintf("push_back_consumer %d", i),
			&merge_with_label.PushBackQueueConsumer{
				Queue:         producerQueue,
				PushBackQueue: producerPushBackQueue,
				Logger:        &logger,
			}); err != nil {
			log.Error().Err(err).Msg("unable to add consumer")
			return
		}
	}

	select {
	case err := <-errChan:
		log.Error().Err(err).Send()
	case <-ctx.Done():
	}
	log.Info().Msg("shutting down")
	_ = server.Shutdown(ctx)
	<-consumerConnection.StopAllConsuming()
}
