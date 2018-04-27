package gorm

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"time"
	"github.com/aws/aws-xray-sdk-go/xray"
)

// SQL opens a normalized and traced wrapper around an *sql.DB connection.
// It uses `sql.Open` internally and shares the same function signature.
// To ensure passwords are filtered, it is HIGHLY RECOMMENDED that your DSN
// follows the format: `<schema>://<user>:<password>@<host>:<port>/<database>`
func SQL(driver, dsn string) (*xrayDB, error) {
	rawDB, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}

	db := &xrayDB{db: rawDB}

	// Detect if DSN is a URL or not, set appropriate attribute
	urlDsn := dsn
	if !strings.Contains(dsn, "//") {
		urlDsn = "//" + urlDsn
	}
	// Here we're trying to detect things like `host:port/database` as a URL, which is pretty hard
	// So we just assume that if it's got a scheme, a user, or a query that it's probably a URL
	if u, err := url.Parse(urlDsn); err == nil && (u.Scheme != "" || u.User != nil || u.RawQuery != "" || strings.Contains(u.Path, "@")) {
		// Check that this isn't in the form of user/pass@host:port/db, as that will shove the host into the path
		if strings.Contains(u.Path, "@") {
			u, _ = url.Parse(fmt.Sprintf("%s//%s%%2F%s", u.Scheme, u.Host, u.Path[1:]))
		}

		// Strip password from user:password pair in address
		if u.User != nil {
			uname := u.User.Username()

			// Some drivers use "user/pass@host:port" instead of "user:pass@host:port"
			// So we must manually attempt to chop off a potential password.
			// But we can skip this if we already found the password.
			if _, ok := u.User.Password(); !ok {
				uname = strings.Split(uname, "/")[0]
			}

			u.User = url.User(uname)
		}

		// Strip password from query parameters
		q := u.Query()
		q.Del("password")
		u.RawQuery = q.Encode()

		db.url = u.String()
		if !strings.Contains(dsn, "//") {
			db.url = db.url[2:]
		}
	} else {
		// We don't *think* it's a URL, so now we have to try our best to strip passwords from
		// some unknown DSL. We attempt to detect whether it's space-delimited or semicolon-delimited
		// then remove any keys with the name "password" or "pwd". This won't catch everything, but
		// from surveying the current (Jan 2017) landscape of drivers it should catch most.
		db.connectionString = stripPasswords(dsn)
	}

	// Detect database type and use that to populate attributes
	var detectors []func(*xrayDB) error
	switch driver {
	case "postgres":
		detectors = append(detectors, postgresDetector)
	case "mysql":
		detectors = append(detectors, mysqlDetector)
	default:
		detectors = append(detectors, postgresDetector, mysqlDetector, mssqlDetector, oracleDetector)
	}
	for _, detector := range detectors {
		if detector(db) == nil {
			break
		}
		db.databaseType = "Unknown"
		db.databaseVersion = "Unknown"
		db.user = "Unknown"
		db.dbname = "Unknown"
	}

	// There's no standard to get SQL driver version information
	// So we invent an interface by which drivers can provide us this data
	type versionedDriver interface {
		Version() string
	}

	d := db.db.Driver()
	if vd, ok := d.(versionedDriver); ok {
		db.driverVersion = vd.Version()
	} else {
		t := reflect.TypeOf(d)
		for t.Kind() == reflect.Ptr {
			t = t.Elem()
		}
		db.driverVersion = t.PkgPath()
	}

	return db, nil
}

// DB copies the interface of sql.DB but adds X-Ray tracing.
// It must be created with xray.SQL.
type xrayDB struct {
	db *sql.DB

	connectionString string
	url              string
	databaseType     string
	databaseVersion  string
	driverVersion    string
	user             string
	dbname           string
}

// Close closes a database and returns error if any.
func (db *xrayDB) Close() error { return db.db.Close() }

// Driver returns database's underlying driver.
func (db *xrayDB) Driver() driver.Driver { return db.db.Driver() }

// Stats returns database statistics.
func (db *xrayDB) Stats() sql.DBStats { return db.db.Stats() }

// SetConnMaxLifetime sets the maximum amount of time a connection may be reused.
func (db *xrayDB) SetConnMaxLifetime(d time.Duration) { db.db.SetConnMaxLifetime(d) }

// SetMaxIdleConns sets the maximum number of connections in the idle connection pool.
func (db *xrayDB) SetMaxIdleConns(n int) { db.db.SetMaxIdleConns(n) }

// SetMaxOpenConns sets the maximum number of open connections to the database.
func (db *xrayDB) SetMaxOpenConns(n int) { db.db.SetMaxOpenConns(n) }

func (db *xrayDB) populate(ctx context.Context, query string) {
	seg := xray.GetSegment(ctx)

	seg.Lock()
	seg.Namespace = "remote"
	seg.GetSQL().ConnectionString = db.connectionString
	seg.GetSQL().URL = db.url
	seg.GetSQL().DatabaseType = db.databaseType
	seg.GetSQL().DatabaseVersion = db.databaseVersion
	seg.GetSQL().DriverVersion = db.driverVersion
	seg.GetSQL().User = db.user
	seg.GetSQL().SanitizedQuery = query
	seg.Unlock()
}

// Tx copies the interface of sql.Tx but adds X-Ray tracing.
// It must be created with xray.DB.Begin.
type Tx struct {
	db *xrayDB
	tx *sql.Tx
}

// Commit commits the transaction.
func (tx *Tx) Commit() error { return tx.tx.Commit() }

// Rollback aborts the transaction.
func (tx *Tx) Rollback() error { return tx.tx.Rollback() }

// Stmt copies the interface of sql.Stmt but adds X-Ray tracing.
// It must be created with xray.DB.Prepare or xray.Tx.Stmt.
type Stmt struct {
	db    *xrayDB
	stmt  *sql.Stmt
	query string
}

// Close closes the statement.
func (stmt *Stmt) Close() error { return stmt.stmt.Close() }

func (stmt *Stmt) populate(ctx context.Context, query string) {
	stmt.db.populate(ctx, query)

	seg := xray.GetSegment(ctx)
	seg.Lock()
	seg.GetSQL().Preparation = "statement"
	seg.Unlock()
}

func postgresDetector(db *xrayDB) error {
	db.databaseType = "Postgres"
	row := db.db.QueryRow("SELECT version(), current_user, current_database()")
	return row.Scan(&db.databaseVersion, &db.user, &db.dbname)
}

func mysqlDetector(db *xrayDB) error {
	db.databaseType = "MySQL"
	row := db.db.QueryRow("SELECT version(), current_user(), database()")
	return row.Scan(&db.databaseVersion, &db.user, &db.dbname)
}

func mssqlDetector(db *xrayDB) error {
	db.databaseType = "MS SQL"
	row := db.db.QueryRow("SELECT @@version, current_user, db_name()")
	return row.Scan(&db.databaseVersion, &db.user, &db.dbname)
}

func oracleDetector(db *xrayDB) error {
	db.databaseType = "Oracle"
	row := db.db.QueryRow("SELECT version FROM v$instance UNION SELECT user, ora_database_name FROM dual")
	return row.Scan(&db.databaseVersion, &db.user, &db.dbname)
}

func stripPasswords(dsn string) string {
	var (
		tok        bytes.Buffer
		res        bytes.Buffer
		isPassword bool
		inBraces   bool
		delimiter  byte = ' '
	)
	flush := func() {
		if inBraces {
			return
		}
		if !isPassword {
			res.Write(tok.Bytes())
		}
		tok.Reset()
		isPassword = false
	}
	if strings.Count(dsn, ";") > strings.Count(dsn, " ") {
		delimiter = ';'
	}

	buf := strings.NewReader(dsn)
	for c, err := buf.ReadByte(); err == nil; c, err = buf.ReadByte() {
		tok.WriteByte(c)
		switch c {
		case ':', delimiter:
			flush()
		case '=':
			tokStr := strings.ToLower(tok.String())
			isPassword = `password=` == tokStr || `pwd=` == tokStr
			if b, err := buf.ReadByte(); err == nil && b == '{' {
				inBraces = true
			}
			buf.UnreadByte()
		case '}':
			b, err := buf.ReadByte()
			if err != nil {
				break
			}
			if b == '}' {
				tok.WriteByte(b)
			} else {
				inBraces = false
				buf.UnreadByte()
			}
		}
	}
	inBraces = false
	flush()
	return res.String()
}

// Begin starts a transaction.
func (db *xrayDB) Begin(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	tx, err := db.db.BeginTx(ctx, opts)
	return &Tx{db, tx}, err
}

// Prepare creates a prepared statement for later queries or executions.
func (db *xrayDB) Prepare(ctx context.Context, query string) (*Stmt, error) {
	stmt, err := db.db.PrepareContext(ctx, query)
	return &Stmt{db, stmt, query}, err
}

// Ping traces verifying a connection to the database is still alive,
// establishing a connection if necessary and adds corresponding information into subsegment.
func (db *xrayDB) Ping(ctx context.Context) error {
	return xray.Capture(ctx, db.dbname, func(ctx context.Context) error {
		db.populate(ctx, "PING")
		return db.db.PingContext(ctx)
	})
}

// Exec captures executing a query without returning any rows and
// adds corresponding information into subsegment.
func (db *xrayDB) Exec(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	var res sql.Result

	err := xray.Capture(ctx, db.dbname, func(ctx context.Context) error {
		db.populate(ctx, query)

		var err error
		res, err = db.db.ExecContext(ctx, query, args...)
		return err
	})

	return res, err
}

// Query captures executing a query that returns rows and adds corresponding information into subsegment.
func (db *xrayDB) Query(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	var res *sql.Rows

	err := xray.Capture(ctx, db.dbname, func(ctx context.Context) error {
		db.populate(ctx, query)

		var err error
		res, err = db.db.QueryContext(ctx, query, args...)
		return err
	})

	return res, err
}

// QueryRow captures executing a query that is expected to return at most one row
// and adds corresponding information into subsegment.
func (db *xrayDB) QueryRow(ctx context.Context, query string, args ...interface{}) *sql.Row {
	var res *sql.Row

	xray.Capture(ctx, db.dbname, func(ctx context.Context) error {
		db.populate(ctx, query)

		res = db.db.QueryRowContext(ctx, query, args...)
		return nil
	})

	return res
}

// Exec captures executing a query that doesn't return rows and adds
// corresponding information into subsegment.
func (tx *Tx) Exec(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	var res sql.Result

	err := xray.Capture(ctx, tx.db.dbname, func(ctx context.Context) error {
		tx.db.populate(ctx, query)

		var err error
		res, err = tx.tx.ExecContext(ctx, query, args...)
		return err
	})

	return res, err
}

// Query captures executing a query that returns rows and adds corresponding information into subsegment.
func (tx *Tx) Query(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	var res *sql.Rows

	err := xray.Capture(ctx, tx.db.dbname, func(ctx context.Context) error {
		tx.db.populate(ctx, query)

		var err error
		res, err = tx.tx.QueryContext(ctx, query, args...)
		return err
	})

	return res, err
}

// QueryRow captures executing a query that is expected to return at most one row and adds
// corresponding information into subsegment.
func (tx *Tx) QueryRow(ctx context.Context, query string, args ...interface{}) *sql.Row {
	var res *sql.Row

	xray.Capture(ctx, tx.db.dbname, func(ctx context.Context) error {
		tx.db.populate(ctx, query)

		res = tx.tx.QueryRowContext(ctx, query, args...)
		return nil
	})

	return res
}

// Stmt returns a transaction-specific prepared statement from an existing statement.
func (tx *Tx) Stmt(ctx context.Context, stmt *Stmt) *Stmt {
	return &Stmt{stmt.db, tx.tx.StmtContext(ctx, stmt.stmt), stmt.query}
}

// Exec captures executing a prepared statement with the given arguments and
// returning a Result summarizing the effect of the statement and adds corresponding
// information into subsegment.
func (stmt *Stmt) Exec(ctx context.Context, args ...interface{}) (sql.Result, error) {
	var res sql.Result

	err := xray.Capture(ctx, stmt.db.dbname, func(ctx context.Context) error {
		stmt.populate(ctx, stmt.query)

		var err error
		res, err = stmt.stmt.ExecContext(ctx, args...)
		return err
	})

	return res, err
}

// Query captures executing a prepared query statement with the given arguments
// and returning the query results as a *Rows and adds corresponding information
// into subsegment.
func (stmt *Stmt) Query(ctx context.Context, args ...interface{}) (*sql.Rows, error) {
	var res *sql.Rows

	err := xray.Capture(ctx, stmt.db.dbname, func(ctx context.Context) error {
		stmt.populate(ctx, stmt.query)

		var err error
		res, err = stmt.stmt.QueryContext(ctx, args...)
		return err
	})

	return res, err
}

// QueryRow captures executing a prepared query statement with the given arguments and
// adds corresponding information into subsegment.
func (stmt *Stmt) QueryRow(ctx context.Context, args ...interface{}) *sql.Row {
	var res *sql.Row

	xray.Capture(ctx, stmt.db.dbname, func(ctx context.Context) error {
		stmt.populate(ctx, stmt.query)

		res = stmt.stmt.QueryRowContext(ctx, args...)
		return nil
	})

	return res
}
