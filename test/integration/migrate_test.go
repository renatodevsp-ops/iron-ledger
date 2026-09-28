package integration

import (
	"context"
	"testing"

	"github.com/ironledger/ironledger/internal/platform/postgres"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Running the migration chain against a database that is already migrated is the
// normal case: every deploy after the first does it. A bug in the "which
// migrations are already applied" query is invisible to a test that only ever
// migrates a fresh database, and it stops the process from starting at all.
func TestUpIsIdempotentOnAnAlreadyMigratedDatabase(t *testing.T) {
	ctx := context.Background()

	ran, err := postgres.Up(ctx, sharedPool)
	require.NoError(t, err, "a second Up must not fail")
	assert.Equal(t, 0, ran, "nothing should be left to apply")

	ran, err = postgres.Up(ctx, sharedPool)
	require.NoError(t, err)
	assert.Equal(t, 0, ran)

	applied := countRows(t, `SELECT count(*) FROM schema_migrations`)
	assert.Equal(t, 10, applied, "the chain is ten migrations")
}

// The checksum guard is what makes an edited migration an error instead of a
// silent divergence between the repository and the database.
func TestUpRefusesAReappliedChecksumDrift(t *testing.T) {
	ctx := context.Background()
	_, err := postgres.Up(ctx, sharedPool)
	require.NoError(t, err)

	// Pretend the recorded checksum disagrees with the file. This simulates an
	// edit to an already-applied migration, which must be refused.
	_, err = sharedPool.Exec(ctx,
		`UPDATE schema_migrations SET checksum = 'tampered' WHERE name = (SELECT min(name) FROM schema_migrations)`)
	require.NoError(t, err)

	_, err = postgres.Up(ctx, sharedPool)
	require.Error(t, err, "checksum drift must be a hard error, not a silent re-apply")
	assert.Contains(t, err.Error(), "modified after it was applied")

	// Restore, so this test does not poison the rest of the package.
	_, err = postgres.Up(ctx, sharedPool)
	require.Error(t, err)
	_, err = sharedPool.Exec(ctx, `UPDATE schema_migrations SET checksum = '' WHERE checksum = 'tampered'`)
	require.NoError(t, err)
	require.NoError(t, repairChecksum(ctx))
}

// repairChecksum recomputes the recorded checksums from the embedded files.
func repairChecksum(ctx context.Context) error {
	all, err := postgres.EmbeddedMigrations()
	if err != nil {
		return err
	}
	for _, m := range all {
		if _, err := sharedPool.Exec(ctx,
			`UPDATE schema_migrations SET checksum = $1 WHERE name = $2`, m.Checksum, m.Name); err != nil {
			return err
		}
	}
	return nil
}
