package goloquent

import (
	"bytes"
	"database/sql"
	"fmt"
	"log"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type mysql struct {
	sequel
}

const minVersion = "5.7"

var _ Dialect = new(mysql)

func init() {
	RegisterDialect("mysql", new(mysql))
}

// Open :
func (s *mysql) Open(conf Config) (*sql.DB, error) {
	addr, buf := "@", new(strings.Builder)
	buf.WriteString(conf.Username + ":" + conf.Password)
	if conf.UnixSocket != "" {
		addr += fmt.Sprintf("unix(%s)", conf.UnixSocket)
	} else {
		host, port := "localhost", "3306"
		if conf.Host != "" {
			host = conf.Host
		}
		if conf.Port != "" {
			port = conf.Port
		}
		addr += fmt.Sprintf("tcp(%s:%s)", host, port)
	}
	buf.WriteString(addr)
	buf.WriteString(fmt.Sprintf("/%s", conf.Database))
	buf.WriteString("?parseTime=true")
	buf.WriteString("&charset=utf8mb4&collation=utf8mb4_unicode_ci")
	log.Println("Connection String :", buf.String())
	client, err := sql.Open("mysql", buf.String())
	if err != nil {
		return nil, err
	}
	return client, nil
}

// Version :
func (s mysql) Version() (version string) {
	verRgx := regexp.MustCompile(`(\d\.\d)`)
	s.db.QueryRow("SELECT VERSION();").Scan(&version)
	log.Println("MySQL version :", version)
	if compareVersion(verRgx.FindStringSubmatch(version)[0], minVersion) > 0 {
		panic(fmt.Errorf("require at least %s version of mysql", minVersion))
	}
	return
}

// Quote :
func (s mysql) Quote(n string) string {
	return fmt.Sprintf("`%s`", n)
}

// Bind :
func (s mysql) Bind(uint) string {
	return "?"
}

// DataType :
func (s mysql) DataType(sc Schema) string {
	buf := new(bytes.Buffer)
	buf.WriteString(sc.DataType)
	if sc.IsUnsigned {
		buf.WriteString(" UNSIGNED")
	}
	if sc.CharSet.Encoding != "" && sc.CharSet.Collation != "" {
		buf.WriteString(fmt.Sprintf(" CHARACTER SET %s COLLATE %s",
			s.Quote(sc.CharSet.Encoding),
			s.Quote(sc.CharSet.Collation)))
	}
	if !sc.IsNullable {
		buf.WriteString(" NOT NULL")
		t := reflect.TypeOf(sc.DefaultValue)
		if t != reflect.TypeOf(OmitDefault(nil)) {
			buf.WriteString(fmt.Sprintf(" DEFAULT %s", s.ToString(sc.DefaultValue)))
		}
	}
	return buf.String()
}

func (s mysql) OnConflictUpdate(table string, cols []string) string {
	buf := new(bytes.Buffer)
	buf.WriteString("ON DUPLICATE KEY UPDATE ")
	for _, c := range cols {
		buf.WriteString(fmt.Sprintf("%s=VALUES(%s),", s.Quote(c), s.Quote(c)))
	}
	buf.Truncate(buf.Len() - 1)
	return buf.String()
}

func (s mysql) CreateTable(table string, columns []Column) error {
	buf := new(strings.Builder)
	buf.WriteString("CREATE TABLE IF NOT EXISTS ")
	buf.WriteString(s.GetTable(table))
	buf.WriteString(" (")

	for _, col := range columns {
		schema := s.GetSchema(col)
		buf.WriteString(s.Quote(schema.Name))
		buf.WriteString(" ")
		buf.WriteString(s.DataType(schema))
		buf.WriteString(",")
		if schema.IsIndexed {
			idx := fmt.Sprintf("%s_%s_%s", table, schema.Name, "Idx")
			buf.WriteString("INDEX ")
			buf.WriteString(s.Quote(idx))
			buf.WriteString(" (")
			buf.WriteString(s.Quote(schema.Name))
			buf.WriteString(")")
			buf.WriteString(",")
		}
	}

	buf.WriteString("PRIMARY KEY (")
	buf.WriteString(s.Quote(pkColumn))
	buf.WriteString(")")
	buf.WriteString(") ENGINE=InnoDB DEFAULT CHARSET=")
	buf.WriteString(s.Quote(s.db.CharSet.Encoding))
	buf.WriteString(" COLLATE=")
	buf.WriteString(s.Quote(s.db.CharSet.Collation))
	buf.WriteString(";")

	return s.db.ExecStmt(&Stmt{
		query: buf,
	})
}

func (s *mysql) AlterTable(table string, columns []Column) error {
	cols := newDictionary(s.GetColumns(table))
	idxs := newDictionary(s.GetIndexes(table))

	buf := new(strings.Builder)
	buf.WriteString("ALTER TABLE ")
	buf.WriteString(s.GetTable(table))
	buf.WriteString(" ")

	suffix := "FIRST"
	for _, col := range columns {
		schema := s.GetSchema(col)
		action := "ADD"
		if cols.has(schema.Name) {
			action = "MODIFY"
		}

		buf.WriteString(action)
		buf.WriteString(" ")
		buf.WriteString(s.Quote(schema.Name))
		buf.WriteString(" ")
		buf.WriteString(s.DataType(schema))
		buf.WriteString(" ")
		buf.WriteString(suffix)
		buf.WriteString(",")

		if schema.IsIndexed {
			idx := fmt.Sprintf("%s_%s_%s", table, schema.Name, "idx")
			if idxs.has(idx) {
				idxs.delete(idx)
			} else {
				buf.WriteString(fmt.Sprintf(" ADD INDEX %s (%s),",
					s.Quote(idx), s.Quote(schema.Name)))
			}
		}
		suffix = fmt.Sprintf("AFTER %s", s.Quote(schema.Name))
		cols.delete(schema.Name)
	}

	for _, col := range cols.keys() {
		buf.WriteString("DROP COLUMN ")
		buf.WriteString(s.Quote(col))
		buf.WriteString(", ")
	}

	for _, idx := range idxs.keys() {
		buf.WriteString("DROP INDEX ")
		buf.WriteString(s.Quote(idx))
		buf.WriteString(", ")
	}

	buf.WriteString("CHARACTER SET ")
	buf.WriteString(s.Quote(s.db.CharSet.Encoding))
	buf.WriteString("COLLATE ")
	buf.WriteString(s.Quote(s.db.CharSet.Collation))
	buf.WriteString(";")

	return s.db.ExecStmt(&Stmt{
		query: buf,
	})
}

func (s mysql) ToString(it interface{}) string {
	var v string
	switch vi := it.(type) {
	case string:
		v = fmt.Sprintf("%q", vi)
	case bool:
		v = fmt.Sprintf("%t", vi)
	case uint, uint8, uint16, uint32, uint64:
		v = fmt.Sprintf("%d", vi)
	case int, int8, int16, int32, int64:
		v = fmt.Sprintf("%d", vi)
	case float32:
		v = strconv.FormatFloat(float64(vi), 'f', -1, 64)
	case float64:
		v = strconv.FormatFloat(vi, 'f', -1, 64)
	case time.Time:
		v = fmt.Sprintf(`"%s"`, vi.Format("2006-01-02 15:04:05"))
	// case json.RawMessage:
	case []interface{}:
		v = fmt.Sprintf(`"%s"`, "[]")
	case map[string]interface{}:
		v = fmt.Sprintf(`"%s"`, "{}")
	case nil:
		v = "NULL"
	default:
		v = fmt.Sprintf("%v", vi)
	}
	return v
}

func (s mysql) UpdateWithLimit() bool {
	return true
}

func (s mysql) ReplaceInto(src, dst string) error {
	src, dst = s.GetTable(src), s.GetTable(dst)
	buf := new(strings.Builder)
	buf.WriteString("REPLACE INTO ")
	buf.WriteString(dst)
	buf.WriteString(" SELECT * FROM ")
	buf.WriteString(src)
	buf.WriteString(";")
	return s.db.ExecStmt(&Stmt{
		query: buf,
	})
}
