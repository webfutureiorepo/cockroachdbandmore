// Copyright 2016 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tests_test

import (
	"context"
	gosql "database/sql"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/ccl"
	"github.com/cockroachdb/cockroach/pkg/internal/rsg"
	"github.com/cockroachdb/cockroach/pkg/internal/sqlsmith"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/builtins"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/builtins/builtinsregistry"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/testutils/datapathutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/skip"
	"github.com/cockroachdb/cockroach/pkg/util/allstacks"
	"github.com/cockroachdb/cockroach/pkg/util/ctxgroup"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/cockroachdb/cockroach/pkg/util/ring"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

var (
	flagRSGTime                    = flag.Duration("rsg", 0, "random syntax generator test duration")
	flagRSGGoRoutines              = flag.Int("rsg-routines", 1, "number of Go routines executing random statements in each RSG test")
	flagRSGExecTimeout             = flag.Duration("rsg-exec-timeout", 35*time.Second, "timeout duration when executing a statement")
	flagRSGExecColumnChangeTimeout = flag.Duration("rsg-exec-column-change-timeout", 50*time.Second, "timeout duration when executing a statement for random column changes")
)

func verifyFormat(sql string) error {
	stmts, err := parser.Parse(sql)
	if err != nil {
		// Cannot serialize a statement list without parsing it.
		return nil //nolint:returnerrcheck
	}
	formattedSQL := stmts.StringWithFlags(tree.FmtShowPasswords)
	formattedStmts, err := parser.Parse(formattedSQL)
	if err != nil {
		return errors.Wrapf(err, "cannot parse output of Format: sql=%q, formattedSQL=%q", sql, formattedSQL)
	}
	formattedFormattedSQL := formattedStmts.StringWithFlags(tree.FmtShowPasswords)
	if formattedSQL != formattedFormattedSQL {
		return errors.Errorf("Parse followed by Format is not idempotent: %q -> %q != %q", sql, formattedSQL, formattedFormattedSQL)
	}
	// TODO(eisen): ensure that the reconstituted SQL not only parses but also has
	// the same meaning as the original.
	return nil
}

type verifyFormatDB struct {
	db              *gosql.DB
	verifyFormatErr error
	mu              struct {
		syncutil.Mutex
		// active holds the currently executing statements.
		active map[string]int
		// lastStmtBuffer contains the last set of statements executed
		// within a ring buffer.
		lastStmtBuffer ring.Buffer[string]
		// lastStatementsDumped indicates if the statements have already been
		// dumped.
		lastStatementsDumped bool
		// lastCompletedStmt tracks the time when the last statement finished
		// executing, which will be used for resettable timeouts.
		lastCompletedStmt time.Time
	}
}

// dumpLastStatements dumps out diagnostic information of currently active and the past 50 queries
// that were executed.
func (db *verifyFormatDB) dumpLastStatements(printFn func(format string, args ...any)) {
	db.mu.Lock()
	defer db.mu.Unlock()
	// Only dump this information once inside the test.
	if db.mu.lastStatementsDumped {
		return
	}
	db.mu.lastStatementsDumped = true
	for i := 0; i < db.mu.lastStmtBuffer.Len(); i++ {
		// Assuming a fully populated buffer, start from the insertion
		// point which will be the oldest statement (if the buffer is fully
		// filled).
		if len(db.mu.lastStmtBuffer.Get(i)) == 0 {
			continue
		}
		printFn("Last executed (%d): %s", i, db.mu.lastStmtBuffer.Get(i))
	}
	// Next dump the set of active statements.
	printFn("Currently active statements: %v", db.mu.active)
}

// Incr records sql in the active map and returns a func to decrement it.
func (db *verifyFormatDB) Incr(sql string) func() {
	db.mu.Lock()
	const MaxStatementBufferSize = 50
	if db.mu.active == nil {
		db.mu.active = make(map[string]int)
		db.mu.lastStmtBuffer = ring.MakeBuffer(make([]string, MaxStatementBufferSize))
	}
	if db.mu.lastStmtBuffer.Len() == MaxStatementBufferSize {
		db.mu.lastStmtBuffer.RemoveFirst()
	}
	db.mu.lastStmtBuffer.AddLast(sql)
	db.mu.active[sql]++
	db.mu.Unlock()

	return func() {
		db.mu.Lock()
		db.mu.active[sql]--
		db.mu.lastCompletedStmt = timeutil.Now()
		if db.mu.active[sql] == 0 {
			delete(db.mu.active, sql)
		}
		db.mu.Unlock()
	}
}

type crasher struct {
	sql    string
	err    error
	detail string
}

func (c *crasher) Error() string {
	return fmt.Sprintf("server panic: %s", c.err)
}

type nonCrasher struct {
	sql string
	err error
}

func (c *nonCrasher) Error() string {
	return c.err.Error()
}
func (db *verifyFormatDB) exec(t *testing.T, ctx context.Context, sql string) error {
	return db.execWithTimeout(t, ctx, sql, *flagRSGExecTimeout)
}
func (db *verifyFormatDB) execWithTimeout(
	t *testing.T, ctx context.Context, sql string, duration time.Duration,
) error {
	return db.execWithResettableTimeout(t,
		ctx,
		sql,
		duration,
		0 /* no resets allowed */)
}

// execWithResettableTimeout executes a statement with a timeout, if the type of
// timeout is resettable then the timeout will be reset everytime a query completes.
// This specifically is used in cases where multiple things might be serially
// executed, for example schema changes on the same table. maxResets can be used
// to guarantee we don't endlessly extend the timeout.
func (db *verifyFormatDB) execWithResettableTimeout(
	t *testing.T, ctx context.Context, sql string, duration time.Duration, maxResets int,
) error {
	if err := func() (retErr error) {
		defer func() {
			if err := recover(); err != nil {
				retErr = errors.CombineErrors(
					errors.AssertionFailedf("panic executing %s: err %v", sql, err),
					retErr,
				)
			}
		}()
		if err := verifyFormat(sql); err != nil {
			db.verifyFormatErr = err
			return err
		}
		return nil
	}(); err != nil {
		return err
	}

	defer db.Incr(sql)()

	var cancel context.CancelCauseFunc
	ctx, cancel = context.WithCancelCause(ctx)
	defer cancel(nil)

	funcdone := make(chan error, 1)
	go func() {
		_, err := db.db.ExecContext(ctx, sql)
		funcdone <- err
	}()
	retry := true
	targetDuration := duration
	cancellationChannel := ctx.Done()
	for retry {
		retry = false
		err := func() error {
			select {
			case err := <-funcdone:
				if err != nil {
					if pqerr := (*pq.Error)(nil); errors.As(err, &pqerr) {
						// Output Postgres error code if it's available.
						if pgcode.MakeCode(string(pqerr.Code)) == pgcode.CrashShutdown {
							return &crasher{
								sql:    sql,
								err:    err,
								detail: pqerr.Detail,
							}
						}
					}
					if es := err.Error(); strings.Contains(es, "internal error") ||
						strings.Contains(es, "driver: bad connection") ||
						strings.Contains(es, "unexpected error inside CockroachDB") {
						return &crasher{
							sql:    sql,
							err:    err,
							detail: pgerror.FullError(err),
						}
					}
					return &nonCrasher{sql: sql, err: err}
				}
				return nil
			case <-cancellationChannel:
				// Sanity: The context is cancelled when the test is about to
				// timeout. We will log whatever statement we're waiting on for
				// debugging purposes. Sometimes queries won't respect
				// cancellation due to lib/pq limitations.
				t.Logf("Context cancelled while executing: %q", sql)
				// We will intentionally retry, which will us to wait for the
				// go routine to complete above to avoid leaking it.
				retry = true
				cancellationChannel = nil
				return nil
			case <-time.After(targetDuration):
				db.mu.Lock()
				defer db.mu.Unlock()
				// In the resettable mode, we are going to wait for no progress on any
				// queries before declaring this a hang.
				if maxResets > 0 {
					if db.mu.lastCompletedStmt.Add(duration).After(timeutil.Now()) {
						// Recompute the timeout duration based, so that the timeout is
						// N seconds after the last queries completion. This is done to
						// the timeouts between queries more reasonable for long intervals:
						// (1) => Executes work in 1 second (setting the last completed query)
						// (2) => Times out after 2 minutes
						// If we simply wait the duration for (2) then we will incur another
						// 2 minute wait and miss potential hangs (if the test times out first).
						// Whereas this approach will wait 2 minutes after the completion of
						// (1), only waiting an extra second more.
						remainingDurationSinceLastStmt := db.mu.lastCompletedStmt.Add(duration).Sub(timeutil.Now())
						targetDuration = duration - remainingDurationSinceLastStmt
						// Avoid having super tight spins, wait at least a second.
						if targetDuration <= time.Second {
							targetDuration = time.Second
						}
						retry = true
						maxResets -= 1
						return nil
					}
				}
				cancel(errors.Newf("cancelling query after %v", duration))
				select {
				case <-funcdone:
					return nil
				case <-time.After(5 * time.Second):
					t.Logf("didn't respect context cancellation within 5 seconds: %s", sql)
				}
				b := allstacks.GetWithBuf(make([]byte, 1024*1024))
				t.Logf("%s\n", b)
				// Now see if we can execute a SELECT 1. This is useful because sometimes an
				// exec timeout is because of a slow-executing statement, and other times
				// it's because the server is completely wedged. This is an automated way
				// to find out.
				errch := make(chan error, 1)
				go func() {
					rows, err := db.db.Query(`SELECT 1`)
					if err == nil {
						rows.Close()
					}
					errch <- err
				}()
				select {
				case <-time.After(5 * time.Second):
					t.Log("SELECT 1 timeout: probably a wedged server")
				case err := <-errch:
					if err != nil {
						t.Log("SELECT 1 execute error:", err)
					} else {
						t.Log("SELECT 1 executed successfully: probably a slow statement")
					}
				}
				return &crasher{
					sql:    sql,
					err:    errors.Newf("statement exec timeout"),
					detail: fmt.Sprintf("timeout: %q. currently executing: %v", sql, db.mu.active),
				}
			}
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

func TestRandomSyntaxGeneration(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	const rootStmt = "stmt"

	testRandomSyntax(t, false, "ident", nil, func(ctx context.Context, db *verifyFormatDB, r *rsg.RSG) error {
		s := r.Generate(rootStmt, 20)
		// Don't start transactions since closing them is tricky. Just issuing a
		// ROLLBACK after all queries doesn't work due to the parellel uses of db,
		// which can start another immediately after the ROLLBACK and cause problems
		// for the following statement. The CREATE DATABASE below would fail with
		// errors about an aborted transaction and thus panic.
		if strings.HasPrefix(s, "BEGIN") || strings.HasPrefix(s, "START") {
			return errors.New("transactions are unsupported")
		}
		if strings.HasPrefix(s, "SET SESSION CHARACTERISTICS AS TRANSACTION") {
			return errors.New("setting session characteristics is unsupported")
		}
		if strings.HasPrefix(s, "DROP DATABASE") {
			return errors.New("dropping the database is likely to timeout since it needs to drop a lot of dependent objects")
		}
		if strings.Contains(s, "READ ONLY") || strings.Contains(s, "read_only") {
			return errors.New("READ ONLY settings are unsupported")
		}
		if strings.Contains(s, "REVOKE") || strings.Contains(s, "GRANT") {
			return errors.New("REVOKE and GRANT are unsupported")
		}
		if strings.Contains(s, "EXPERIMENTAL SCRUB DATABASE SYSTEM") {
			return errors.New("See #43693")
		}
		// Recreate the database on every run in case it was renamed in
		// a previous run. Should always succeed.
		if err := db.exec(t, ctx, `CREATE DATABASE IF NOT EXISTS ident`); err != nil {
			return err
		}
		return db.exec(t, ctx, s)
	})
}

func TestRandomSyntaxSelect(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	const rootStmt = "target_list"

	testRandomSyntax(t, false, "ident", func(ctx context.Context, db *verifyFormatDB, r *rsg.RSG) error {
		return db.exec(t, ctx, `CREATE DATABASE IF NOT EXISTS ident; CREATE TABLE IF NOT EXISTS ident.ident (ident decimal);`)
	}, func(ctx context.Context, db *verifyFormatDB, r *rsg.RSG) error {
		targets := r.Generate(rootStmt, 300)
		var where, from string
		// Only generate complex clauses half the time.
		if rand.Intn(2) == 0 {
			where = r.Generate("where_clause", 300)
			from = r.Generate("from_clause", 300)
		} else {
			from = "FROM ident"
		}
		s := fmt.Sprintf("SELECT %s %s %s", targets, from, where)
		return db.exec(t, ctx, s)
	})
}

type namedBuiltin struct {
	name    string
	builtin tree.Overload
}

func TestRandomSyntaxFunctions(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	done := make(chan struct{})
	defer close(done)
	namedBuiltinChan := make(chan namedBuiltin)
	go func() {
		for {
			for _, name := range builtins.AllBuiltinNames() {
				lower := strings.ToLower(name)
				if strings.HasPrefix(lower, "crdb_internal.force_") {
					continue
				}
				switch lower {
				case "pg_sleep":
					continue
				case "crdb_internal.create_sql_schema_telemetry_job":
					// We can create a crazy number of telemtry jobs accidentally,
					// within the test. Leading to terrible contention.
					continue
				case "crdb_internal.gen_rand_ident":
					// Generates random identifiers, so a large number are dangerous and
					// can take a long time.
					continue
				case "st_frechetdistance", "st_buffer":
					// Some spatial function are slow and testing them here
					// is not worth it.
					continue
				case "trigger_in", "trigger_out":
					// Skip trigger I/O functions since we can't generate random trigger
					// arguments.
					continue
				case "crdb_internal.reset_sql_stats",
					"crdb_internal.check_consistency",
					"crdb_internal.request_statement_bundle",
					"crdb_internal.reset_activity_tables",
					"crdb_internal.revalidate_unique_constraints_in_all_tables",
					"crdb_internal.scan_storage_internal_keys",
					"crdb_internal.validate_ttl_scheduled_jobs",
					"crdb_internal.fingerprint":
					// Skipped due to long execution time.
					continue
				}
				_, variations := builtinsregistry.GetBuiltinProperties(name)
				for _, builtin := range variations {
					select {
					case <-done:
						return
					case namedBuiltinChan <- namedBuiltin{name: name, builtin: builtin}:
					}
				}
			}
		}
	}()

	testRandomSyntax(t, false, "defaultdb", nil, func(ctx context.Context, db *verifyFormatDB, r *rsg.RSG) error {
		nb := <-namedBuiltinChan
		var args []string
		switch ft := nb.builtin.Types.(type) {
		case tree.ParamTypes:
			for _, arg := range ft {
				// CollatedString's default has no Locale, and so GenerateRandomArg will panic
				// on RandDatumWithNilChance. Copy the typ and fake a locale.
				typ := *arg.Typ
				if typ.Locale() == "" && typ.Family() == types.CollatedStringFamily {
					locale := "en_US"
					typ.InternalType.Locale = &locale
				}
				args = append(args, r.GenerateRandomArg(&typ))
			}
		case tree.HomogeneousType:
			for i := r.Intn(5); i > 0; i-- {
				var typ *types.T
				switch r.Intn(4) {
				case 0:
					typ = types.String
				case 1:
					typ = types.Float
				case 2:
					typ = types.Bool
				case 3:
					typ = types.TimestampTZ
				}
				args = append(args, r.GenerateRandomArg(typ))
			}
		case tree.VariadicType:
			for _, t := range ft.FixedTypes {
				args = append(args, r.GenerateRandomArg(t))
			}
			for i := r.Intn(5); i > 0; i-- {
				args = append(args, r.GenerateRandomArg(ft.VarType))
			}
		default:
			panic(errors.AssertionFailedf("unknown fn.Types: %T", ft))
		}
		var limit string
		switch strings.ToLower(nb.name) {
		case "generate_series":
			limit = " LIMIT 100"
		}
		s := fmt.Sprintf("SELECT %s(%s) %s", nb.name, strings.Join(args, ", "), limit)
		// Use a re-settable timeout since in concurrent scenario some operations may
		// involve schema changes like truncates. In general this should make
		// this test more resilient as the timeouts are reset as long progress
		// is made on *some* connection.
		return db.execWithResettableTimeout(t, ctx, s, *flagRSGExecTimeout, *flagRSGGoRoutines)
	})
}

func TestRandomSyntaxFuncCommon(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	const rootStmt = "func_expr_common_subexpr"

	testRandomSyntax(t, false, "defaultdb", nil, func(ctx context.Context, db *verifyFormatDB, r *rsg.RSG) error {
		expr := r.Generate(rootStmt, 30)
		s := fmt.Sprintf("SELECT %s", expr)
		return db.exec(t, ctx, s)
	})
}

func TestRandomSyntaxSchemaChangeDatabase(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	roots := []string{
		"create_database_stmt",
		"drop_database_stmt",
		"alter_rename_database_stmt",
		"create_user_stmt",
		"drop_user_stmt",
		"alter_user_stmt",
	}
	// Create multiple databases, so that in concurrent scenarios two connections
	// will always share the same database.
	numDatabases := max(*flagRSGGoRoutines/2, 1)
	databases := make([]string, 0, numDatabases)
	for dbIdx := 0; dbIdx < numDatabases; dbIdx++ {
		databases = append(databases, fmt.Sprintf("ident_%d", dbIdx))
	}
	var nextDatabaseName atomic.Int64
	testRandomSyntax(t, true, "ident", func(ctx context.Context, db *verifyFormatDB, r *rsg.RSG) error {
		if err := db.exec(t, ctx, "SET CLUSTER SETTING sql.catalog.descriptor_lease_duration = '30s'"); err != nil {
			return err
		}
		for _, dbName := range databases {
			if err := db.exec(t, ctx, fmt.Sprintf(`CREATE DATABASE %s;`, dbName)); err != nil {
				return err
			}
		}
		return nil
	}, func(ctx context.Context, db *verifyFormatDB, r *rsg.RSG) error {
		n := r.Intn(len(roots))
		s := r.Generate(roots[n], 30)
		// Select a database and use it in the generated statement.
		dbName := databases[nextDatabaseName.Add(1)%int64(len(databases))]
		return db.exec(t, ctx, strings.Replace(s, "ident", dbName, -1))
	})
}

func TestRandomSyntaxSchemaChangeColumn(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	numTables := *flagRSGGoRoutines
	roots := []string{
		"alter_table_cmd",
	}

	// The goroutines will use round-robin to pick the next table to modify.
	tableIDMu := syncutil.Mutex{}
	tableID := 0
	incrementTableID := func() int {
		tableIDMu.Lock()
		defer tableIDMu.Unlock()
		tableID++
		if tableID >= numTables {
			tableID = 0
		}
		return tableID
	}

	testRandomSyntax(t, true, "ident", func(ctx context.Context, db *verifyFormatDB, r *rsg.RSG) error {
		if err := db.exec(t, ctx, "SET CLUSTER SETTING sql.catalog.descriptor_lease_duration = '30s'"); err != nil {
			return err
		}
		if err := db.exec(t, ctx, `CREATE DATABASE ident;`); err != nil {
			return err
		}
		for i := 0; i < numTables; i++ {
			if err := db.exec(t, ctx, fmt.Sprintf(`CREATE TABLE ident.ident%d (ident decimal);`, i)); err != nil {
				return err
			}
		}
		return nil
	}, func(ctx context.Context, db *verifyFormatDB, r *rsg.RSG) error {
		n := r.Intn(len(roots))
		s := fmt.Sprintf("ALTER TABLE ident.ident%d %s", incrementTableID(), r.Generate(roots[n], 500))
		// Execute with a resettable timeout, where we allow up to N go-routines worth
		// of resets. This should be the maximum theoretical time we can get
		// stuck behind other work.
		return db.execWithResettableTimeout(t,
			ctx,
			s,
			*flagRSGExecColumnChangeTimeout,
			*flagRSGGoRoutines)
	})
}

var ignoredErrorPatterns = []string{
	"unimplemented",
	"unsupported binary operator",
	"unsupported comparison operator",
	"memory budget exceeded",
	"set-returning functions are not allowed in",
	"txn already encountered an error; cannot be used anymore",
	"no data source matches prefix",
	"index .* already contains column",
	"cannot convert .* to .*",
	"index .* is in used as unique constraint",
	"could not decorrelate subquery",
	"column reference .* is ambiguous",
	"INSERT has more expressions than target columns",
	"index .* is in use as unique constraint",
	"frame .* offset must not be .*",
	"bit string length .* does not match type",
	"column reference .* not allowed in this context",
	"cannot write directly to computed column",
	"index .* in the middle of being added",
	"could not mark job .* as succeeded",
	"failed to read backup descriptor",
	"AS OF SYSTEM TIME: timestamp before 1970-01-01T00:00:00Z is invalid",
	"BACKUP for requested time  needs option 'revision_history'",
	"RESTORE timestamp: supplied backups do not cover requested time",

	// Numeric conditions
	"exponent out of range",
	"result out of range",
	"argument out of range",
	"integer out of range",
	"invalid operation",
	"invalid mask",
	"cannot take square root of a negative number",
	"out of int64 range",
	"underflow, subnormal",
	"overflow",
	"requested length too large",
	"division by zero",
	"is out of range",

	// Type checking
	"value type .* doesn't match type .* of column",
	"incompatible value type",
	"incompatible COALESCE expressions",
	"error type checking constant value",
	"ambiguous binary operator",
	"ambiguous call",
	"cannot be matched",
	"unknown signature",
	"cannot determine type of empty array",
	"conflicting ColumnTypes",

	// Data dependencies
	"violates not-null constraint",
	"violates unique constraint",
	"column .* is referenced by the primary key",
	"column .* is referenced by existing index",

	// Context-specific string formats
	"invalid regexp flag",
	"unrecognized privilege",
	"invalid escape string",
	"error parsing regexp",
	"could not parse .* as type bytes",
	"UUID must be exactly 16 bytes long",
	"unsupported timespan",
	"does not exist",
	"unterminated string",
	"incorrect UUID length",
	"the input string must not be empty",

	// JSON builtins
	"mismatched array dimensions",
	"cannot get array length of a non-array",
	"cannot get array length of a scalar",
	"cannot be called on a non-array",
	"cannot call json_object_keys on an array",
	"cannot set path in scalar",
	"cannot delete path in scalar",
	"path element at position .* is null",
	"path element is not an integer",
	"cannot delete from object using integer index",
	"invalid concatenation of jsonb objects",
	"null value not allowed for object key",

	// Builtins that have funky preconditions
	"cannot delete from scalar",
	"lastval is not yet defined",
	"negative substring length",
	"non-positive substring length",
	"bit strings of different sizes",
	"inet addresses with different sizes",
	"zero length IP",
	"values of different sizes",
	"must have even number of elements",
	"cannot take logarithm of a negative number",
	"input value must be",
	"formats are supported for decode",
	"only available in ccl",
	"expect comma-separated list of filename",
	"unknown constraint",
	"invalid destination encoding name",
	"invalid IP format",
	"invalid format code",
	`.*val\(\): syntax error`,
	`.*val\(\): syntax error at or near`,
	`.*val\(\): help token in input`,
	"invalid source encoding name",
	"strconv.Atoi: parsing .*: invalid syntax",
	"field position .* must be greater than zero",
	"cannot take logarithm of zero",
	"only 'hex', 'escape', and 'base64' formats are supported for encode",
	"LIKE pattern must not end with escape character",

	// TODO(mjibson): fix these
	"column .* must appear in the GROUP BY clause or be used in an aggregate function",
	"aggregate functions are not allowed in ON",
	"ordered-set aggregations must have a WITHIN GROUP clause containing one ORDER BY column",
}

var ignoredRegex = regexp.MustCompile(strings.Join(ignoredErrorPatterns, "|"))

func TestRandomSyntaxSQLSmith(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	defer ccl.TestingEnableEnterprise()() // allow usage of partitions

	var smither *sqlsmith.Smither

	tableStmts := make([]string, 0)
	testRandomSyntax(t, true, "defaultdb", func(ctx context.Context, db *verifyFormatDB, r *rsg.RSG) error {
		if err := db.exec(t, ctx, "SET CLUSTER SETTING sql.catalog.descriptor_lease_duration = '30s'"); err != nil {
			return err
		}
		setups := []string{sqlsmith.RandTableSetupName, "seed"}
		for _, s := range setups {
			randTables := sqlsmith.Setups[s](r.Rnd)
			for _, stmt := range randTables {
				if err := db.exec(t, ctx, stmt); err != nil {
					return err
				}
				tableStmts = append(tableStmts, stmt)
				t.Logf("%s;", stmt)
			}

		}
		var err error
		smither, err = sqlsmith.NewSmither(db.db, r.Rnd, sqlsmith.DisableMutations())
		return err
	}, func(ctx context.Context, db *verifyFormatDB, r *rsg.RSG) error {
		s := smither.Generate()
		err := db.exec(t, ctx, s)
		if c := (*crasher)(nil); errors.As(err, &c) {
			if err := db.exec(t, ctx, "USE defaultdb"); err != nil {
				t.Fatalf("couldn't reconnect to db after crasher: %v", c)
			}
			t.Logf("CRASHER:\ncaused by: %s\n\nSTATEMENT:\n%s;\n\nserver stacktrace:\n%s\n", c.Error(), s, c.detail)
			return c
		}
		if err == nil {
			return nil
		}
		msg := err.Error()
		shouldLogErr := true
		if ignoredRegex.MatchString(msg) {
			shouldLogErr = false
		}
		if testing.Verbose() && shouldLogErr {
			t.Logf("ERROR: %s\ncaused by:\n%s;\n", err, s)
		}
		return err
	})
	if smither != nil {
		smither.Close()
	}

	t.Logf("To reproduce, use schema:\n")
	for _, stmt := range tableStmts {
		t.Logf("%s;", stmt)
	}
	t.Log()
}

func TestRandomDatumRoundtrip(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ec := eval.MakeTestingEvalContext(nil)

	var smither *sqlsmith.Smither
	testRandomSyntax(t, true, "", func(ctx context.Context, db *verifyFormatDB, r *rsg.RSG) error {
		var err error
		smither, err = sqlsmith.NewSmither(nil, r.Rnd)
		return err
	}, func(ctx context.Context, db *verifyFormatDB, r *rsg.RSG) error {
		defer func() {
			if err := recover(); err != nil {
				s := fmt.Sprint(err)
				// JSONB NaN and Infinity can't round
				// trip because JSON doesn't support
				// those as Numbers, only strings. (Try
				// `JSON.stringify(Infinity)` in a JS console.)
				if strings.Contains(s, "JSONB") && (strings.Contains(s, "Infinity") || strings.Contains(s, "NaN")) {
					return
				}
				for _, cmp := range []string{
					"ReturnType called on TypedExpr with empty typeAnnotation",
					"runtime error: invalid memory address or nil pointer dereference",
				} {
					if strings.Contains(s, cmp) {
						return
					}
				}
				panic(err)
			}
		}()
		generated := smither.GenerateExpr()
		typ := generated.ResolvedType()
		switch typ {
		case types.Date, types.Decimal:
			return nil
		}
		serializedGen := tree.Serialize(generated)

		sema := tree.MakeSemaContext(nil /* resolver */)
		// We don't care about errors below because they are often
		// caused by sqlsmith generating bogus queries. We're just
		// looking for datums that don't match.
		parsed1, err := parser.ParseExpr(serializedGen)
		if err != nil {
			return nil //nolint:returnerrcheck
		}
		typed1, err := parsed1.TypeCheck(ctx, &sema, typ)
		if err != nil {
			return nil //nolint:returnerrcheck
		}
		datum1, err := eval.Expr(ctx, &ec, typed1)
		if err != nil {
			return nil //nolint:returnerrcheck
		}
		serialized1 := tree.Serialize(datum1)

		parsed2, err := parser.ParseExpr(serialized1)
		if err != nil {
			return nil //nolint:returnerrcheck
		}
		typed2, err := parsed2.TypeCheck(ctx, &sema, typ)
		if err != nil {
			return nil //nolint:returnerrcheck
		}
		datum2, err := eval.Expr(ctx, &ec, typed2)
		if err != nil {
			return nil //nolint:returnerrcheck
		}
		serialized2 := tree.Serialize(datum2)

		if serialized1 != serialized2 {
			panic(errors.Errorf("serialized didn't match:\nexpr: %s\nfirst: %s\nsecond: %s", generated, serialized1, serialized2))
		}
		if cmp, err := datum1.Compare(ctx, &ec, datum2); err != nil {
			panic(err)
		} else if cmp != 0 {
			panic(errors.Errorf("%s [%[1]T] != %s [%[2]T] (original expr: %s)", serialized1, serialized2, serializedGen))
		}
		return nil
	})
}

// testRandomSyntax performs all of the RSG setup and teardown for common
// random syntax testing operations. It takes a closure where the random
// expression should be generated and executed. It returns an error indicating
// if the statement executed successfully. This is used to verify that at
// least 1 success occurs (otherwise it is likely a bad test).
func testRandomSyntax(
	t *testing.T,
	allowDuplicates bool,
	databaseName string,
	setup func(context.Context, *verifyFormatDB, *rsg.RSG) error,
	fn func(context.Context, *verifyFormatDB, *rsg.RSG) error,
) {
	if *flagRSGTime == 0 {
		skip.IgnoreLint(t, "enable with '-rsg <duration>'")
	}
	ctx := context.Background()

	var params base.TestServerArgs
	params.UseDatabase = databaseName
	// Catch panics and return them as errors.
	params.Knobs.PGWireTestingKnobs = &sql.PGWireTestingKnobs{
		CatchPanics: true,
	}
	srv, rawDB, _ := serverutils.StartServer(t, params)
	defer srv.Stopper().Stop(ctx)
	db := &verifyFormatDB{db: rawDB}
	// If the test fails we can log the previous set of statements.
	defer func() {
		if t.Failed() {
			db.dumpLastStatements(t.Logf)
		}
	}()
	err := db.exec(t, ctx, "SET CLUSTER SETTING schemachanger.job.max_retry_backoff='1s'")
	require.NoError(t, err)

	// Disable the test object generator. This merely causes the built-in function to report
	// an error when called. This is OK -- we are testing syntax, so this will still ensure
	// the function syntax is exercised.
	err = db.exec(t, ctx, "SET CLUSTER SETTING sql.schema.test_object_generator.enabled = false")
	require.NoError(t, err)

	yBytes, err := os.ReadFile(datapathutils.TestDataPath(t, "rsg", "sql.y"))
	if err != nil {
		t.Fatal(err)
	}
	_, seed := randutil.NewTestRand()
	r, err := rsg.NewRSG(seed, string(yBytes), allowDuplicates)
	if err != nil {
		t.Fatal(err)
	}

	if setup != nil {
		err := setup(ctx, db, r)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Broadcast channel for all workers.
	done := make(chan struct{})
	time.AfterFunc(*flagRSGTime, func() {
		close(done)
	})
	var countsMu struct {
		syncutil.Mutex
		total, success int
	}
	ctx, cancel := context.WithCancel(ctx)
	// Print status updates. We want this go routine to continue until all the
	// workers are done, even if their ctx has been canceled, so the ctx for
	// this func is a separate one with its own cancel.
	go func(ctx context.Context) {
		start := timeutil.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			countsMu.Lock()
			t.Logf("%v of %v: %d executions, %d successful",
				timeutil.Since(start).Round(time.Second),
				*flagRSGTime,
				countsMu.total,
				countsMu.success,
			)
			countsMu.Unlock()
		}
	}(ctx)
	ctx, timeoutCancel := context.WithTimeout(ctx, *flagRSGTime)
	err = ctxgroup.GroupWorkers(ctx, *flagRSGGoRoutines, func(ctx context.Context, _ int) error {
		for {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			err := fn(ctx, db, r)
			countsMu.Lock()
			countsMu.total++
			if err == nil {
				countsMu.success++
			} else {
				if c := (*crasher)(nil); errors.As(err, &c) {
					// NOTE: Changes to this output format must be kept in-sync
					// with logic in CondensedMessage.RSGCrash in order for
					// crashes to be correctly reported to Github.
					t.Errorf("Crash detected: %s\n%s;\n\nMore details:\n%s", c.Error(), c.sql, c.detail)
				}
			}
			countsMu.Unlock()
		}
	})
	timeoutCancel()
	// cancel the timer printing's ctx
	cancel()
	t.Logf("%d executions, %d successful", countsMu.total, countsMu.success)
	if err != nil {
		t.Fatal(err)
	}
	if countsMu.success == 0 {
		t.Fatal("0 successful executions")
	}
	if db.verifyFormatErr != nil {
		t.Error(db.verifyFormatErr)
	}
}
