package pgqueue_test

import (
	"context"
	"os"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/pgqueue"
)

// sharedStore is a single Store instance shared across all tests in this
// package, initialized once in TestMain. A shared container cuts total
// test time from ~20 min (1 container per test) to ~1 min.
var sharedStore *pgqueue.Store

func TestMain(m *testing.M) {
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image: "supabase/postgres:17.6.1.151",
		Env: map[string]string{
			"POSTGRES_PASSWORD": "test",
			"POSTGRES_DB":       "testdb",
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
	dsn := "postgres://supabase_admin:test@" + host + ":" + port.Port() + "/testdb?sslmode=disable"

	sharedStore, err = pgqueue.New(ctx, dsn)
	if err != nil {
		panic("connect: " + err.Error())
	}
	if err := sharedStore.Migrate(ctx); err != nil {
		panic("migrate: " + err.Error())
	}

	code := m.Run()

	sharedStore.Close()
	_ = ctr.Terminate(ctx)
	os.Exit(code)
}
