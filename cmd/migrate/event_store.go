package migrate

import (
	"context"
	_ "embed"
	"io"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/zitadel/logging"

	"github.com/zitadel/zitadel/internal/database"
	"github.com/zitadel/zitadel/internal/database/dialect"
	"github.com/zitadel/zitadel/internal/eventstore"
	es_old "github.com/zitadel/zitadel/internal/eventstore/repository/sql"
	es_v3 "github.com/zitadel/zitadel/internal/eventstore/v3"
	"github.com/zitadel/zitadel/internal/id"
)

var shouldIgnorePrevious bool

func eventstoreCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eventstore",
		Short: "migrates the eventstore of an instance from one database to another",
		Long: `migrates the eventstore of an instance from one database to another
ZITADEL needs to be initialized
Migrate only copies events2 and unique constraints`,
		Run: func(cmd *cobra.Command, args []string) {
			config := mustNewMigrationConfig(viper.GetViper())
			copyEventstore(cmd.Context(), config)
		},
	}

	cmd.Flags().BoolVar(&shouldReplace, "replace-constraints", false, "allow delete unique constraints of defined instances before copy")
	cmd.Flags().BoolVar(&shouldIgnorePrevious, "ignore-previous", false, "ignores previous migrations of the events table")

	return cmd
}

func copyEventstore(ctx context.Context, config *Migration) {
	sourceClient, err := database.Connect(config.Source, false, dialect.DBPurposeQuery)
	logging.OnError(err).Fatal("unable to connect to source database")
	defer sourceClient.Close()

	destClient, err := database.Connect(config.Destination, false, dialect.DBPurposeEventPusher)
	logging.OnError(err).Fatal("unable to connect to destination database")
	defer destClient.Close()

	copyEvents(ctx, sourceClient, destClient)
	copyUniqueConstraints(ctx, sourceClient, destClient)
}

func positionQuery(db *database.DB) string {
	switch db.Type() {
	case "postgres":
		return "SELECT EXTRACT(EPOCH FROM clock_timestamp())"
	case "cockroach":
		return "SELECT cluster_logical_timestamp()"
	default:
		logging.WithFields("db_type", db.Type()).Fatal("database type not recognized")
		return ""
	}
}

func copyEvents(ctx context.Context, source, dest *database.DB) {
	start := time.Now()
	reader, writer := io.Pipe()
	errs := make(chan error, 1)

	migrationID, err := id.SonyFlakeGenerator().Next()
	logging.OnError(err).Fatal("unable to generate migration id")

	sourceConn, err := source.Conn(ctx)
	logging.OnError(err).Fatal("unable to acquire source connection")

	destConn, err := dest.Conn(ctx)
	logging.OnError(err).Fatal("unable to acquire dest connection")

	es := eventstore.NewEventstore(&eventstore.Config{
		Pusher:  es_v3.NewEventstore(source),
		Querier: es_old.NewCRDB(source),
	})

	previousMigration, err := queryLastSuccessfulMigration(ctx, es, dest.DatabaseName())
	logging.OnError(err).Fatal("unable to query latest successful migration")

	maxPosition, err := writeMigrationStart(ctx, es, migrationID, dest.DatabaseName())
	logging.OnError(err).Fatal("unable to write migration started event")

	// get position
	pos := make(chan float64)

	logging.WithFields("from", previousMigration.position, "to", maxPosition).Info("start event migration")

	go func() {
		position := strconv.FormatFloat(<-pos, 'E', -1, 64)
		err := sourceConn.Raw(func(driverConn interface{}) error {
			conn := driverConn.(*stdlib.Conn).Conn()
			// TODO: sql injection
			_, err := conn.PgConn().CopyTo(ctx, writer, "COPY (SELECT instance_id, aggregate_type, aggregate_id, event_type, sequence, revision, created_at,  regexp_replace(payload::TEXT, '\\\\u0000', '', 'g')::JSON payload, creator, owner, (SELECT "+position+"::DECIMAL) AS position, row_number() OVER (PARTITION BY instance_id ORDER BY position, in_tx_order) AS in_tx_order FROM eventstore.events2 "+instanceClause()+" AND position <= (SELECT "+strconv.FormatFloat(maxPosition, 'E', -1, 64)+"::DECIMAL) AND position > (SELECT "+strconv.FormatFloat(previousMigration.position, 'E', -1, 64)+"::DECIMAL) ORDER BY instance_id, position, in_tx_order) TO STDOUT")
			writer.Close()
			return err
		})
		errs <- err
	}()

	var eventCount int64
	err = destConn.Raw(func(driverConn interface{}) error {
		conn := driverConn.(*stdlib.Conn).Conn()
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		row := tx.QueryRow(ctx, positionQuery(dest))
		var position float64
		if err := row.Scan(&position); err != nil {
			return err
		}
		_ = tx.Commit(ctx)
		pos <- position

		tag, err := conn.PgConn().CopyFrom(ctx, reader, "COPY eventstore.events2 FROM STDIN")
		eventCount = tag.RowsAffected()

		return err
	})

	writeCopyEventsDone(ctx, es, dest.DatabaseName(), migrationID, <-errs, err)

	logging.WithFields("took", time.Since(start), "count", eventCount).Info("events migrated")
}

func decryptEventPayload(ctx context.Context, source *database.DB, maxPosition float64) {
	err := createDecryptedEventsTable(ctx, source)
	logging.OnError(err).Fatal("unable to create decrypted events table in source")

}

/*
idpintent.saml.succeeded: {assertion}
idpintent.succeeded: idp{idpAccessToken}

instance.idp.oauth.added: {clientSecret}
instance.idp.oidc.added: {clientSecret}
instance.idp.oidc.migrated.azure: {client_secret}
instance.idp.oidc.migrated.google: {clientSecret}
instance.idp.azure.added: {client_secret}
instance.idp.github.added: {clientSecret}
instance.idp.github_enterprise.added: {clientSecret}
instance.idp.gitlab.added: {client_secret}
instance.idp.gitlab_self_hosted.added: {client_secret}
instance.idp.google.added: {clientSecret}
instance.idp.ldap.v2.added: {bindPassword}
instance.idp.apple.added: {privateKey}
instance.idp.saml.added: {key} //TODO: do we need to decrypt the key?

iam.idp.oidc.config.added: {clientSecret}

org.idp.oauth.added: {clientSecret}
org.idp.oidc.added: {clientSecret}
org.idp.oidc.migrated.azure: {client_secret}
org.idp.oidc.migrated.google: {clientSecret}
org.idp.azure.added: {client_secret}
org.idp.github.added: {clientSecret}
org.idp.github_enterprise.added: {clientSecret}
org.idp.gitlab.added: {client_secret}
org.idp.gitlab_self_hosted.added: {client_secret}
org.idp.google.added: {clientSecret}
org.idp.ldap.v2.added: {bindPassword}
org.idp.apple.added: {privateKey}
org.idp.saml.added: {key} //TODO: do we need to decrypt the key?

instance.sms.configtwilio.added: {token}
instance.sms.configtwilio.token.changed: {token}

instance.smtp.config.password.changed: {password}
instance.smtp.config.added: {password}

user.human.mfa.otp.added: {otpSecret}

key_pair.added: {publicKey}
key_pair.certificate.added: {certificate}

// TODO: internal/api/saml/certificate.go
*/

//go:embed sql/decrypted_payload_table.sql
var decryptedPayloadTable string

func createDecryptedEventsTable(ctx context.Context, db *database.DB) error {
	_, err := db.ExecContext(ctx, decryptedPayloadTable)
	return err
}

func writeCopyEventsDone(ctx context.Context, es *eventstore.Eventstore, destName string, id string, sourceErr, destErr error) {
	logging.OnError(destErr).Error("unable to copy events to destination")
	logging.OnError(sourceErr).Fatal("unable to copy events from source")

	if destErr != nil {
		err := writeMigrationDone(ctx, es, id, destErr, destName)
		logging.OnError(err).Fatal("unable to write failed event")
		return
	}

	if sourceErr != nil {
		err := writeMigrationDone(ctx, es, id, sourceErr, destName)
		logging.OnError(err).Fatal("unable to write failed event")
		return
	}

	err := writeMigrationDone(ctx, es, id, nil, destName)
	logging.OnError(err).Fatal("unable to write failed event")
}

func copyUniqueConstraints(ctx context.Context, source, dest *database.DB) {
	start := time.Now()
	reader, writer := io.Pipe()
	errs := make(chan error, 1)

	sourceConn, err := source.Conn(ctx)
	logging.OnError(err).Fatal("unable to acquire source connection")

	go func() {
		err := sourceConn.Raw(func(driverConn interface{}) error {
			conn := driverConn.(*stdlib.Conn).Conn()
			// TODO: sql injection
			_, err := conn.PgConn().CopyTo(ctx, writer, "COPY (SELECT instance_id, unique_type, unique_field FROM eventstore.unique_constraints "+instanceClause()+") TO stdout")
			writer.Close()
			return err
		})
		errs <- err
	}()

	destConn, err := dest.Conn(ctx)
	logging.OnError(err).Fatal("unable to acquire dest connection")

	var eventCount int64
	err = destConn.Raw(func(driverConn interface{}) error {
		conn := driverConn.(*stdlib.Conn).Conn()

		if shouldReplace {
			_, err := conn.Exec(ctx, "DELETE FROM eventstore.unique_constraints "+instanceClause())
			if err != nil {
				return err
			}
		}

		tag, err := conn.PgConn().CopyFrom(ctx, reader, "COPY eventstore.unique_constraints FROM stdin")
		eventCount = tag.RowsAffected()

		return err
	})
	logging.OnError(err).Fatal("unable to copy unique constraints to destination")
	logging.OnError(<-errs).Fatal("unable to copy unique constraints from source")
	logging.WithFields("took", time.Since(start), "count", eventCount).Info("unique constraints migrated")
}
