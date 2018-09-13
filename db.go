package goloquent

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"time"

	"cloud.google.com/go/datastore"
)

// TransactionHandler :
type TransactionHandler func(*DB) error

// LogHandler :
type LogHandler func(*Stmt)

// public constant variables :
const (
	pkLen            = 512
	pkColumn         = "$Key"
	softDeleteColumn = "$Deleted"
	keyDelimeter     = "/"
)

// CommonError :
var (
	ErrNoSuchEntity  = fmt.Errorf("goloquent: entity not found")
	ErrInvalidCursor = fmt.Errorf("goloquent: invalid cursor")
)

// Config :
type Config struct {
	Username   string
	Password   string
	Host       string
	Port       string
	Database   string
	UnixSocket string
	CharSet    *CharSet
	Logger     LogHandler
}

// Normalize :
func (c *Config) Normalize() {
	c.Username = strings.TrimSpace(c.Username)
	c.Host = strings.TrimSpace(strings.ToLower(c.Host))
	c.Port = strings.TrimSpace(c.Port)
	c.Database = strings.TrimSpace(c.Database)
	c.UnixSocket = strings.TrimSpace(c.UnixSocket)
	if c.CharSet != nil && c.CharSet.Encoding != "" && c.CharSet.Collation != "" {
		c.CharSet.Collation = strings.TrimSpace(c.CharSet.Collation)
		c.CharSet.Encoding = strings.TrimSpace(c.CharSet.Encoding)
	} else {
		charset := utf8mb4CharSet
		c.CharSet = &charset
	}
}

// Replacer :
type Replacer interface {
	Upsert(model interface{}, k ...*datastore.Key) error
	Save(model interface{}) error
}

// Client :
type Client struct {
	sqlCommon
	CharSet
	dialect Dialect
	logger  LogHandler
}

func (c Client) consoleLog(s *Stmt) {
	if c.logger != nil {
		c.logger(s)
	}
}

func (c Client) prepareExec(query string, args ...interface{}) (sql.Result, error) {
	conn, err := c.sqlCommon.Prepare(query)
	if err != nil {
		return nil, fmt.Errorf("goloquent: unable to prepare sql statement : %v", err)
	}
	defer conn.Close()
	result, err := conn.Exec(args...)
	if err != nil {
		return nil, fmt.Errorf("goloquent: %v", err)
	}
	return result, nil
}

// ExecStmt :
func (c Client) ExecStmt(s *Stmt) error {
	s.startTrace()
	defer func() {
		s.stopTrace()
		//c.consoleLog(s)
	}()
	log.Println(s.Raw())
	result, err := c.prepareExec(s.Raw(), s.Args()...)
	if err != nil {
		return err
	}
	s.Result = result
	return nil
}

// QueryStmt :
func (c Client) QueryStmt(stmt *Stmt) (*sql.Rows, error) {
	stmt.startTrace()
	defer func() {
		stmt.stopTrace()
		// c.consoleLog(ss)
	}()
	log.Println(stmt.Raw())
	var rows, err = c.Query(stmt.Raw(), stmt.Args()...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// QueryRowStmt :
func (c *Client) QueryRowStmt(stmt *Stmt) *sql.Row {
	stmt.startTrace()
	defer func() {
		stmt.stopTrace()
		// c.consoleLog(ss)
	}()
	log.Println(stmt.Raw())
	return c.QueryRow(stmt.Raw(), stmt.Args()...)
}

// Exec :
func (c Client) Exec(query string, args ...interface{}) (sql.Result, error) {
	result, err := c.sqlCommon.Exec(query, args...)
	if err != nil {
		return nil, fmt.Errorf("goloquent: %v", err)
	}
	return result, nil
}

// Query :
func (c Client) Query(query string, args ...interface{}) (*sql.Rows, error) {
	rows, err := c.sqlCommon.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("goloquent: %v", err)
	}
	return rows, nil
}

// QueryRow :
func (c Client) QueryRow(query string, args ...interface{}) *sql.Row {
	return c.sqlCommon.QueryRow(query, args...)
}

// DB :
type DB struct {
	id      string
	driver  string
	name    string
	replica string
	client  Client
	dialect Dialect
	omits   []string
}

// NewDB :
func NewDB(driver string, charset CharSet, conn sqlCommon, dialect Dialect, logHandler LogHandler) *DB {
	client := Client{conn, charset, dialect, logHandler}
	dialect.SetDB(client)
	return &DB{
		id:      fmt.Sprintf("%s:%d", driver, time.Now().UnixNano()),
		driver:  driver,
		name:    dialect.CurrentDB(),
		client:  client,
		dialect: dialect,
	}
}

// clone a new connection
func (db *DB) clone() *DB {
	return &DB{
		id:      db.id,
		driver:  db.driver,
		name:    db.name,
		replica: fmt.Sprintf("%d", time.Now().Unix()),
		client:  db.client,
		dialect: db.dialect,
	}
}

// ID :
func (db DB) ID() string {
	return db.id
}

// Name :
func (db DB) Name() string {
	return db.name
}

// NewQuery :
func (db *DB) NewQuery() *Query {
	return newQuery(db)
}

// Query :
func (db *DB) Query(stmt string, args ...interface{}) (*sql.Rows, error) {
	return db.client.Query(stmt, args...)
}

// Exec :
func (db *DB) Exec(stmt string, args ...interface{}) (sql.Result, error) {
	return db.client.Exec(stmt, args...)
}

// Table :
func (db *DB) Table(name string) *Table {
	return &Table{name, db}
}

// Migrate :
func (db *DB) Migrate(model ...interface{}) error {
	return newBuilder(db.NewQuery()).migrate(model)
}

// Omit :
func (db *DB) Omit(fields ...string) Replacer {
	ff := newDictionary(fields)
	clone := db.clone()
	ff.delete(keyFieldName)
	ff.delete(pkColumn)
	clone.omits = ff.keys()
	return clone
}

// Create :
func (db *DB) Create(model interface{}, parentKey ...*datastore.Key) error {
	if parentKey == nil {
		return newBuilder(db.NewQuery()).put(model, nil)
	}
	return newBuilder(db.NewQuery()).put(model, parentKey)
}

// Upsert :
func (db *DB) Upsert(model interface{}, parentKey ...*datastore.Key) error {
	if parentKey == nil {
		return newBuilder(db.NewQuery().Omit(db.omits...)).upsert(model, nil)
	}
	return newBuilder(db.NewQuery().Omit(db.omits...)).upsert(model, parentKey)
}

// Save :
func (db *DB) Save(model interface{}) error {
	if err := checkSinglePtr(model); err != nil {
		return err
	}
	return newBuilder(db.NewQuery().Omit(db.omits...)).save(model)
}

// Delete :
func (db *DB) Delete(model interface{}) error {
	return newBuilder(db.NewQuery()).delete(model, true)
}

// Destroy :
func (db *DB) Destroy(model interface{}) error {
	return newBuilder(db.NewQuery()).delete(model, false)
}

// Truncate :
func (db *DB) Truncate(model ...interface{}) error {
	ns := make([]string, 0, len(model))
	for _, m := range model {
		var table string
		v := reflect.Indirect(reflect.ValueOf(m))
		switch v.Type().Kind() {
		case reflect.String:
			table = v.String()
		case reflect.Struct:
			table = v.Type().Name()
		default:
			return errors.New("goloquent: unsupported model")
		}

		table = strings.TrimSpace(table)
		if table == "" {
			return errors.New("goloquent: missing table name")
		}
		ns = append(ns, table)
	}
	return newBuilder(db.NewQuery()).truncate(ns...)
}

// Select :
func (db *DB) Select(fields ...string) *Query {
	return db.NewQuery().Select(fields...)
}

// Find :
func (db *DB) Find(key *datastore.Key, model interface{}) error {
	return db.NewQuery().Find(key, model)
}

// First :
func (db *DB) First(model interface{}) error {
	return db.NewQuery().First(model)
}

// Get :
func (db *DB) Get(model interface{}) error {
	return db.NewQuery().Get(model)
}

// Paginate :
func (db *DB) Paginate(p *Pagination, model interface{}) error {
	return db.NewQuery().Paginate(p, model)
}

// Ancestor :
func (db *DB) Ancestor(ancestor *datastore.Key) *Query {
	return db.NewQuery().Ancestor(ancestor)
}

// AnyOfAncestor :
func (db *DB) AnyOfAncestor(ancestors ...*datastore.Key) *Query {
	return db.NewQuery().AnyOfAncestor(ancestors...)
}

// Where :
func (db *DB) Where(field string, operator string, value interface{}) *Query {
	return db.NewQuery().Where(field, operator, value)
}

// RunInTransaction :
func (db *DB) RunInTransaction(cb TransactionHandler) error {
	return newBuilder(db.NewQuery()).runInTransaction(cb)
}

// Close :
func (db *DB) Close() error {
	x, isOk := db.client.sqlCommon.(*sql.DB)
	if !isOk {
		return nil
	}
	return x.Close()
}
