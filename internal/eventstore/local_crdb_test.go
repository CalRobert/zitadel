package eventstore_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach-go/v2/testserver"
	"github.com/zitadel/logging"

	"github.com/zitadel/zitadel/cmd/initialise"
	"github.com/zitadel/zitadel/internal/database"
	"github.com/zitadel/zitadel/internal/database/cockroach"
	"github.com/zitadel/zitadel/internal/eventstore"
	es_sql "github.com/zitadel/zitadel/internal/eventstore/repository/sql"
	new_es "github.com/zitadel/zitadel/internal/eventstore/v3"
)

var (
	testCRDBClient *database.DB
	queriers       map[string]eventstore.Querier = make(map[string]eventstore.Querier)
	pushers        map[string]eventstore.Pusher  = make(map[string]eventstore.Pusher)
)

func TestMain(m *testing.M) {
	ts, err := testserver.NewTestServer()
	if err != nil {
		logging.WithFields("error", err).Fatal("unable to start db")
	}

	testCRDBClient = &database.DB{
		Database: new(testDB),
	}
	testCRDBClient.DB, err = sql.Open("postgres", ts.PGURL().String())
	if err != nil {
		logging.WithFields("error", err).Fatal("unable to connect to db")
	}
	if err = testCRDBClient.Ping(); err != nil {
		logging.WithFields("error", err).Fatal("unable to ping db")
	}

	v2 := &es_sql.CRDB{DB: testCRDBClient}
	queriers["v2"] = v2

	pushers["v3"] = new_es.NewEventstore(testCRDBClient)

	if localDB, err := connectLocalhost(); err == nil {
		if err = initDB(localDB.DB); err != nil {
			logging.WithFields("error", err).Fatal("migrations failed")
		}
		pushers["localhost"] = new_es.NewEventstore(localDB)
	}

	// pushers["v2"] = v2

	defer func() {
		testCRDBClient.Close()
		ts.Stop()
	}()

	if err = initDB(testCRDBClient.DB); err != nil {
		logging.WithFields("error", err).Fatal("migrations failed")
	}

	os.Exit(m.Run())
}

func initDB(db *sql.DB) error {
	initialise.ReadStmts("cockroach")
	config := new(database.Config)
	config.SetConnector(&cockroach.Config{
		User: cockroach.User{
			Username: "zitadel",
		},
		Database: "zitadel",
	})
	err := initialise.Init(db,
		initialise.VerifyUser(config.Username(), ""),
		initialise.VerifyDatabase(config.DatabaseName()),
		initialise.VerifyGrant(config.DatabaseName(), config.Username()))
	if err != nil {
		return err
	}
	return initialise.VerifyZitadel(db, *config)
}

func connectLocalhost() (*database.DB, error) {
	client, err := sql.Open("pgx", "postgresql://root@localhost:26257/defaultdb?sslmode=disable")
	if err != nil {
		return nil, err
	}
	if err = testCRDBClient.Ping(); err != nil {
		return nil, err
	}

	return &database.DB{
		DB: client,
	}, nil
}

type testDB struct{}

func (_ *testDB) Timetravel(time.Duration) string { return " AS OF SYSTEM TIME '-1 ms' " }

func (*testDB) DatabaseName() string { return "db" }

func (*testDB) Username() string { return "user" }

func (*testDB) Type() string { return "type" }

func generateCommand(aggregateType eventstore.AggregateType, aggregateID string, opts ...func(*testEvent)) eventstore.Command {
	e := &testEvent{
		BaseEvent: eventstore.BaseEvent{
			Agg: &eventstore.Aggregate{
				ID:            aggregateID,
				Type:          aggregateType,
				ResourceOwner: "ro",
				Version:       "v1",
			},
			Service:   "svc",
			EventType: "test.created",
		},
	}

	for _, opt := range opts {
		opt(e)
	}

	return e
}

type testEvent struct {
	eventstore.BaseEvent
	uniqueConstraints []*eventstore.UniqueConstraint
}

func (e *testEvent) Payload() any {
	return e.Data
}

func (e *testEvent) UniqueConstraints() []*eventstore.UniqueConstraint {
	return e.uniqueConstraints
}

func canceledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func fillUniqueData(unique_type, field, instanceID string) error {
	_, err := testCRDBClient.Exec("INSERT INTO eventstore.unique_constraints (unique_type, unique_field, instance_id) VALUES ($1, $2, $3)", unique_type, field, instanceID)
	return err
}

func generateAddUniqueConstraint(table, uniqueField string) func(e *testEvent) {
	return func(e *testEvent) {
		e.uniqueConstraints = append(e.uniqueConstraints,
			&eventstore.UniqueConstraint{
				UniqueType:  table,
				UniqueField: uniqueField,
				Action:      eventstore.UniqueConstraintAdd,
			},
		)
	}
}

func generateRemoveUniqueConstraint(table, uniqueField string) func(e *testEvent) {
	return func(e *testEvent) {
		e.uniqueConstraints = append(e.uniqueConstraints,
			&eventstore.UniqueConstraint{
				UniqueType:  table,
				UniqueField: uniqueField,
				Action:      eventstore.UniqueConstraintRemove,
			},
		)
	}
}

func withTestData(data any) func(e *testEvent) {
	return func(e *testEvent) {
		d, err := json.Marshal(data)
		if err != nil {
			panic("marshal data failed")
		}
		e.Data = d
	}
}

func cleanupEventstore() {
	_, err := testCRDBClient.Exec("TRUNCATE eventstore.events")
	if err != nil {
		logging.Warnf("unable to truncate events: %v", err)
	}
}
