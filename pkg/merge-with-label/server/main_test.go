package server_test

import (
	"context"
	"os"
	"testing"

	"github.com/rs/zerolog"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/pgqueue"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/server"
)

// sharedHandler is a Handler backed by one container shared across all
// server tests. It is initialized in TestMain.
var sharedHandler *server.Handler

func TestMain(m *testing.M) {
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image: "supabase/postgres:17.6.1.151",
		Env: map[string]string{
			"POSTGRES_USER":     "test",
			"POSTGRES_PASSWORD": "test",
			"POSTGRES_DB":       "testdb",
		},
		Cmd: []string{
			"-c", "cron.database_name=testdb",
		},
		ExposedPorts: []string{"5432/tcp"},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2),
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		panic("start postgres container: " + err.Error())
	}

	host, err := ctr.Host(ctx)
	if err != nil {
		panic("get host: " + err.Error())
	}
	port, err := ctr.MappedPort(ctx, "5432")
	if err != nil {
		panic("get port: " + err.Error())
	}
	dsn := "postgres://test:test@" + host + ":" + port.Port() + "/testdb?sslmode=disable"

	store, err := pgqueue.New(ctx, dsn)
	if err != nil {
		panic("pgqueue.New: " + err.Error())
	}
	if err := store.Migrate(ctx); err != nil {
		panic("migrate: " + err.Error())
	}

	logger := zerolog.Nop()
	sharedHandler = &server.Handler{
		GetLoggerForContext: func(_ context.Context) *zerolog.Logger { return &logger },
		AllowedRepositories: common.RegexSlice{common.MustNewRegexItem(".*")},
		Store:               store,
		RateLimitInterval:   0,
	}

	code := m.Run()

	store.Close()
	_ = ctr.Terminate(ctx)
	os.Exit(code)
}
