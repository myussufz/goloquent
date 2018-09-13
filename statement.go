package goloquent

import (
	"bytes"
	"database/sql"
	"strings"
	"time"
)

// type stmt struct {
// 	statement *bytes.Buffer
// 	arguments []interface{}
// }

// func (s stmt) string() string {
// 	return s.statement.String()
// }

// func (s stmt) isZero() bool {
// 	return !(s.statement.Len() > 0)
// }

type replacer interface {
	Bind(uint) string
	Value(interface{}) string
}

type writer interface {
	WriteString(string) (int, error)
	String() string
	Len() int
}

// Stmt :
type Stmt struct {
	query     writer
	args      []interface{}
	crud      string
	replacer  replacer
	startTime time.Time
	endTime   time.Time
	Result    sql.Result
}

func (s Stmt) isZero() bool {
	return !(s.query.Len() > 0)
}

func (s *Stmt) startTrace() {
	s.startTime = time.Now().UTC()
}

func (s *Stmt) stopTrace() {
	s.endTime = time.Now().UTC()
}

// TimeElapse :
func (s Stmt) TimeElapse() time.Duration {
	return s.endTime.Sub(s.startTime)
}

// Raw :
func (s *Stmt) Raw() string {
	return s.query.String()
}

// Raw :
// func (s *Stmt) Raw() string {
// 	buf := new(bytes.Buffer)
// 	if len(s.arguments) <= 0 {
// 		return s.string()
// 	}
// 	arr := strings.Split(s.string(), variable)
// 	for i := 0; i < len(arr); i++ {
// 		str := arr[i] + s.replacer.Bind(uint(i+1))
// 		if i >= len(arr)-1 {
// 			str = arr[i]
// 		}
// 		buf.WriteString(str)
// 	}
// 	return buf.String()
// 	return buf.String()
// }

// String :
func (s *Stmt) String() string {
	buf := new(bytes.Buffer)
	arr := strings.Split(s.query.String(), variable)
	for i, aa := range s.args {
		str := arr[i] + s.replacer.Value(aa)
		buf.WriteString(str)
	}
	buf.WriteString(arr[len(arr)-1])
	return buf.String()
}

// Args :
func (s Stmt) Args() []interface{} {
	return s.args
}
