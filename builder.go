package goloquent

import (
	"bytes"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/datastore"
)

const (
	variable      = "?"
	jsonDelimeter = ":"
)

type indexType int

const (
	bTreeIdx indexType = iota
	uniqueIdx
)

type builder struct {
	db    *DB
	query scope
}

func newBuilder(query *Query) *builder {
	clone := query.db.clone()
	return &builder{
		db:    clone,
		query: query.clone().scope,
	}
}

func (b *builder) quoteIfNecessary(v string) string {
	if regexp.MustCompile("^[\\$a-zA-Z\\d]+(\\.[a-zA-Z\\d]+)*$").MatchString(v) {
		return b.db.dialect.Quote(v)
	}
	return v
}

func (b *builder) addIndex(fields []string, idxType indexType) error {
	table := b.query.table
	buf := new(strings.Builder)
	buf.WriteString("CREATE")
	idxName := fmt.Sprintf("%s_%s_idx", table, strings.Join(fields, "_"))
	switch idxType {
	case uniqueIdx:
		idxName = fmt.Sprintf("%s_%s_unique", table, strings.Join(fields, "_"))
		buf.WriteString(" UNIQUE")
	default:
	}
	if b.db.dialect.HasIndex(table, idxName) {
		return nil
	}
	buf.WriteString(fmt.Sprintf(" INDEX %s ON %s (%s);",
		b.db.dialect.Quote(idxName),
		b.db.dialect.GetTable(table),
		b.db.dialect.Quote(strings.Join(fields, ","))))
	return b.db.client.ExecStmt(&Stmt{
		query: buf,
	})
}

func (b *builder) dropTableIfExists(table string) error {
	buf := new(strings.Builder)
	buf.WriteString("DROP TABLE IF EXISTS ")
	buf.WriteString(b.db.dialect.GetTable(table))
	buf.WriteString(";")
	return b.db.client.ExecStmt(&Stmt{
		query: buf,
	})
}

func (b *builder) buildSelect(query scope) *Stmt {
	scope := "*"
	if len(query.projection) > 0 {
		projection := make([]string, len(query.projection), len(query.projection))
		copy(projection, query.projection)
		for i := 0; i < len(query.projection); i++ {
			projection[i] = b.quoteIfNecessary(projection[i])
		}
		scope = strings.Join(projection, ",")
	}
	if len(query.distinctOn) > 0 {
		distinctOn := make([]string, len(query.distinctOn), len(query.distinctOn))
		copy(distinctOn, query.distinctOn)
		for i := 0; i < len(query.distinctOn); i++ {
			distinctOn[i] = b.quoteIfNecessary(distinctOn[i])
		}
		scope = "DISTINCT " + strings.Join(distinctOn, ",")
	}
	buf := new(strings.Builder)
	buf.WriteString("SELECT ")
	buf.WriteString(scope)
	return &Stmt{
		query: buf,
	}
}

func (b *builder) buildWhere(query scope) (*Stmt, error) {
	buf := new(strings.Builder)
	wheres := make([]string, 0)
	args := make([]interface{}, 0)
	for _, f := range query.filters {
		name := b.db.dialect.Quote(f.Field())
		v, err := f.Interface()
		if err != nil {
			return nil, err
		}

		if f.IsJSON() {
			str, vv, err := b.db.dialect.FilterJSON(f)
			if err != nil {
				return nil, err
			}
			wheres = append(wheres, str)
			args = append(args, vv...)
			continue
		}

		switch f.Field() {
		case keyFieldName, pkColumn:
			name = b.db.dialect.Quote(pkColumn)
			v, err = interfaceToKeyString(f.value)
			if err != nil {
				return nil, err
			}
		}

		op, vv := "=", variable
		switch f.operator {
		case Equal:
			if v == nil {
				wheres = append(wheres, fmt.Sprintf("%s IS NULL", name))
				continue
			}
		case EqualTo:
			op = "<=>"
		case NotEqual:
			op = "<>"
			if v == nil {
				wheres = append(wheres, fmt.Sprintf("%s IS NOT NULL", name))
				continue
			}
		case GreaterThan:
			op = ">"
		case GreaterEqual:
			op = ">="
		case LessThan:
			op = "<"
		case LessEqual:
			op = "<="
		case AnyLike:
			x, isOk := v.([]interface{})
			if !isOk {
				x = append(x, v)
			}
			if len(x) <= 0 {
				return nil, fmt.Errorf(`value for "AnyLike" operator cannot be empty`)
			}
			buf := new(bytes.Buffer)
			buf.WriteString("(")
			for j := 0; j < len(x); j++ {
				buf.WriteString(fmt.Sprintf("%s LIKE %s OR ", name, variable))
			}
			buf.Truncate(buf.Len() - 4)
			buf.WriteString(")")

			wheres = append(wheres, buf.String())
			args = append(args, x...)
			continue
		case Like:
			op = "LIKE"
		case NotLike:
			op = "NOT LIKE"
		case In:
			op = "IN"
			x, isOk := v.([]interface{})
			if !isOk {
				x = append(x, v)
			}
			if len(x) <= 0 {
				return nil, fmt.Errorf(`value for "In" operator cannot be empty`)
			}
			vv = fmt.Sprintf("(%s)", strings.TrimRight(
				strings.Repeat(variable+",", len(x)), ","))
			wheres = append(wheres, fmt.Sprintf("%s %s %s", name, op, vv))
			args = append(args, x...)
			continue
		case NotIn:
			op = "NOT IN"
			x, isOk := v.([]interface{})
			if !isOk {
				x = append(x, v)
			}
			if len(x) <= 0 {
				return nil, fmt.Errorf(`value for "NotIn" operator cannot be empty`)
			}
			vv = fmt.Sprintf("(%s)", strings.TrimRight(
				strings.Repeat(variable+",", len(x)), ","))
			wheres = append(wheres, fmt.Sprintf("%s %s %s", name, op, vv))
			args = append(args, x...)
			continue
		}
		wheres = append(wheres, fmt.Sprintf("%s %s %s", name, op, vv))
		args = append(args, v)
	}

	for _, aa := range query.ancestors {
		if aa.isGroup {
			buf := new(bytes.Buffer)
			buf.WriteString("(")
			for _, x := range aa.data {
				buf.WriteString(fmt.Sprintf("%s LIKE %s OR ", b.db.dialect.Quote(pkColumn), variable))
				args = append(args, fmt.Sprintf("%%%s/%%", stringifyKey(x.(*datastore.Key))))
			}
			buf.Truncate(buf.Len() - 4)
			buf.WriteString(")")
			wheres = append(wheres, buf.String())
			continue
		}

		wheres = append(wheres, fmt.Sprintf("%s LIKE %s", b.db.dialect.Quote(pkColumn), variable))
		args = append(args, fmt.Sprintf("%%%s/%%", stringifyKey(aa.data[0].(*datastore.Key))))
	}

	if len(wheres) > 0 {
		buf.WriteString(" WHERE ")
		buf.WriteString(strings.Join(wheres, " AND "))
	} else {
		buf.Reset()
	}

	return &Stmt{
		query: buf,
		args:  args,
	}, nil
}

func (b *builder) buildOrder(query scope) *Stmt {
	buf := new(strings.Builder)
	arr := make([]string, 0, len(query.orders))

	for _, o := range query.orders {
		// __key__ sorting, filter
		name := b.db.dialect.Quote(o.field)
		if o.field == keyFieldName {
			name = b.db.dialect.Quote(pkColumn)
		}
		suffix := " ASC"
		if o.direction != ascending {
			suffix = " DESC"
		}
		arr = append(arr, name+suffix)
	}

	if len(arr) > 0 {
		buf.WriteString(" ORDER BY " + strings.Join(arr, ","))
	}

	return &Stmt{
		query: buf,
	}
}

func (b *builder) buildLimitOffset(query scope) *Stmt {
	buf := new(strings.Builder)
	if query.limit > 0 {
		buf.WriteString(fmt.Sprintf(" LIMIT %d", query.limit))
	}
	if query.offset > 0 {
		buf.WriteString(fmt.Sprintf(" OFFSET %d", query.offset))
	}
	return &Stmt{
		query: buf,
	}
}

func (b *builder) buildStmt(query scope, args ...interface{}) (*Stmt, error) {
	buf := new(strings.Builder)
	stmt, err := b.buildWhere(query)
	if err != nil {
		return nil, err
	}
	if !stmt.isZero() {
		args = append(args, stmt.Args()...)
		buf.WriteString(stmt.Raw())
	}
	buf.WriteString(b.buildOrder(query).Raw())
	buf.WriteString(b.buildLimitOffset(query).Raw())
	return &Stmt{
		query: buf,
		args:  args,
	}, nil
}

func (b *builder) createTable(e *entity) error {
	return b.db.dialect.CreateTable(e.Name(), e.columns)
}

func (b *builder) alterTable(e *entity) error {
	return b.db.dialect.AlterTable(e.Name(), e.columns)
}

func (b *builder) migrate(models []interface{}) error {
	for _, mm := range models {
		e, err := newEntity(mm)
		if err != nil {
			return err
		}
		if b.db.dialect.HasTable(e.Name()) {
			if err := b.alterTable(e); err != nil {
				return err
			}
			continue
		}
		if err := b.createTable(e); err != nil {
			return err
		}
	}
	return nil
}

func (b *builder) getStmt(e *entity) (*Stmt, error) {
	query := b.query
	buf := new(strings.Builder)
	buf.WriteString(b.buildSelect(query).Raw())
	buf.WriteString(" FROM ")
	buf.WriteString(b.db.dialect.GetTable(e.Name()))
	if !query.noScope && e.hasSoftDelete() {
		query.filters = append(query.filters, Filter{
			field:    softDeleteColumn,
			operator: Equal,
			value:    nil,
		})
	}
	stmt, err := b.buildStmt(query)
	if err != nil {
		return nil, err
	}
	buf.WriteString(stmt.Raw())
	switch query.lockMode {
	case ReadLock:
		buf.WriteString(" LOCK IN SHARE MODE")
	case WriteLock:
		buf.WriteString(" FOR UPDATE")
	}
	buf.WriteString(";")

	return &Stmt{
		query: buf,
		args:  stmt.Args(),
	}, nil
}

func (b *builder) run(table string, stmt *Stmt) (*Iterator, error) {
	var rows, err = b.db.client.QueryStmt(stmt)
	if err != nil {
		return nil, fmt.Errorf("goloquent: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("goloquent: %v", err)
	}

	it := Iterator{
		table:    table,
		stmt:     &Stmt{replacer: b.db.dialect},
		position: -1,
		columns:  cols,
	}

	i := 0
	for rows.Next() {
		m := make([]interface{}, len(cols))
		for j := range cols {
			m[j] = &m[j]
		}

		if err := rows.Scan(m...); err != nil {
			return nil, err
		}

		for j, name := range cols {
			it.put(i, name, m[j])
		}
		it.patchKey()
		i++
	}

	return &it, nil
}

func (b *builder) get(model interface{}, mustExist bool) error {
	e, err := newEntity(model)
	if err != nil {
		return err
	}
	e.setName(b.query.table)
	stmt, err := b.getStmt(e)
	if err != nil {
		return err
	}

	it, err := b.run(e.Name(), stmt)
	if err != nil {
		return err
	}

	first := it.First()
	if mustExist && first == nil {
		return ErrNoSuchEntity
	}

	if first != nil {
		err = it.Scan(model)
		if err != nil {
			return err
		}
	} else {
		v := reflect.ValueOf(model)
		vi := reflect.New(v.Type().Elem())
		v.Elem().Set(vi.Elem())
	}
	return nil
}

func (b *builder) getMulti(model interface{}) error {
	e, err := newEntity(model)
	if err != nil {
		return err
	}
	e.setName(b.query.table)
	cmd, err := b.getStmt(e)
	if err != nil {
		return err
	}

	it, err := b.run(e.Name(), cmd)
	if err != nil {
		return err
	}

	v := reflect.Indirect(reflect.ValueOf(model))
	vv := reflect.MakeSlice(v.Type(), 0, 0)
	isPtr, t := checkMultiPtr(v)
	for it.Next() {
		vi := reflect.New(t)
		_, err = it.scan(vi.Interface())
		if err != nil {
			return err
		}
		if !isPtr {
			vi = vi.Elem()
		}
		vv = reflect.Append(vv, vi)
	}
	v.Set(vv)
	return nil
}

func baseToInterface(it interface{}) interface{} {
	var v interface{}
	switch vi := it.(type) {
	case nil, bool, uint64, int64, float64, string:
		v = vi
	case []byte:
		v = string(vi)
	case time.Time:
		v = vi.Format("2006-01-02 15:04:05")
	default:
		v = vi
	}
	return v
}

func (b *builder) paginate(p *Pagination, model interface{}) error {
	// e, err := newEntity(model)
	// if err != nil {
	// 	return err
	// }
	// e.setName(b.query.table)
	// cmds, err := b.getStmt(e)
	// if err != nil {
	// 	return err
	// }

	// oriCmd := *cmds
	// if p.Cursor != "" {
	// 	c, err := DecodeCursor(p.Cursor)
	// 	if err != nil {
	// 		return err
	// 	}
	// 	if sha1Sign(&Stmt{replacer: b.db.dialect}) != c.Signature {
	// 		return ErrInvalidCursor
	// 	}
	// 	query := b.query
	// 	buf, args := new(bytes.Buffer), make([]interface{}, 0)
	// 	buf.WriteString(b.buildSelect(query).Raw())
	// 	buf.WriteString(" FROM ")
	// 	buf.WriteString(b.db.dialect.GetTable(e.Name()))
	// 	cmd, err := b.buildWhere(query)
	// 	if err != nil {
	// 		return err
	// 	}

	// 	orders := query.orders
	// 	projection := make([]string, 0, len(orders))
	// 	for _, o := range orders {
	// 		projection = append(projection, o.field)
	// 	}
	// 	values, or := make([]interface{}, len(orders)), make([]string, 0)
	// 	for i := 0; i < len(values); i++ {
	// 		values[i] = &values[i]
	// 	}
	// 	if !cmd.isZero() {
	// 		args = append(args, cmd.Args()...)
	// 		buf.WriteString(cmd.Raw())
	// 		buf.WriteString(" AND ")
	// 	} else {
	// 		if len(orders) > 0 {
	// 			buf.WriteString(" WHERE ")
	// 		}
	// 	}
	// 	if err := b.db.Table(e.Name()).
	// 		WhereEqual(keyFieldName, c.Key).
	// 		Select(projection...).
	// 		Limit(1).Scan(values...); err != nil {
	// 		return ErrInvalidCursor
	// 	}
	// 	arg := make([]interface{}, 0, len(orders))
	// 	for i, o := range orders {
	// 		vv := baseToInterface(values[i])
	// 		op := ">="
	// 		if o.direction == descending {
	// 			op = "<="
	// 		}
	// 		if i < len(orders)-1 {
	// 			buf.WriteString(fmt.Sprintf("%s %s %s AND ",
	// 				b.db.dialect.Quote(o.field), op, variable))
	// 			args = append(args, vv)
	// 			op = strings.Trim(op, "=")
	// 		}
	// 		or = append(or, fmt.Sprintf("%s %s %s",
	// 			b.db.dialect.Quote(o.field), op, variable))
	// 		arg = append(arg, vv)
	// 	}
	// 	buf.WriteString("(" + strings.Join(or, " OR ") + ")")
	// 	args = append(args, arg...)
	// 	buf.WriteString(b.buildOrder(query).Raw())
	// 	buf.WriteString(b.buildLimitOffset(query).Raw())
	// 	buf.WriteString(";")
	// 	// cmds = &stmt{statement: buf, arguments: args}
	// }

	// it, err := b.run(e.Name(), cmds)
	// if err != nil {
	// 	return err
	// }

	// it.stmt = &Stmt{stmt: oriCmd, replacer: b.db.dialect}
	// i, v := uint(1), reflect.Indirect(reflect.ValueOf(model))
	// vv := reflect.MakeSlice(v.Type(), 0, 0)
	// isPtr, t := checkMultiPtr(v)
	// for it.Next() {
	// 	if i > p.Limit {
	// 		continue
	// 	}
	// 	vi := reflect.New(t)
	// 	_, err = it.scan(vi.Interface())
	// 	if err != nil {
	// 		return err
	// 	}
	// 	cc, _ := it.Cursor()
	// 	p.nxtCursor = cc
	// 	if !isPtr {
	// 		vi = vi.Elem()
	// 	}
	// 	vv = reflect.Append(vv, vi)
	// 	i++
	// }

	// v.Set(vv)
	// count := it.Count()
	// if count <= p.Limit {
	// 	p.nxtCursor = Cursor{}
	// } else {
	// 	count--
	// }
	// p.count = count
	return nil
}

func (b *builder) replaceInto(table string) error {
	buf, args := new(strings.Builder), make([]interface{}, 0)
	buf.WriteString("REPLACE INTO ")
	buf.WriteString(b.db.dialect.GetTable(table))
	buf.WriteString(" ")
	buf.WriteString(b.buildSelect(b.query).Raw())
	buf.WriteString(" FROM " + b.db.dialect.GetTable(b.query.table))
	stmt, err := b.buildWhere(b.query)
	if err != nil {
		return err
	}
	if !stmt.isZero() {
		buf.WriteString(stmt.Raw())
		args = append(args, stmt.Args()...)
	}
	buf.WriteString(";")
	return b.db.client.ExecStmt(&Stmt{
		query: buf,
		args:  args,
	})
}

func (b *builder) putStmt(parentKey []*datastore.Key, e *entity) (*Stmt, error) {
	v := e.slice.Elem()

	isInline := (parentKey == nil && len(parentKey) == 0)
	buf, args := new(bytes.Buffer), make([]interface{}, 0)
	keys := make([]*datastore.Key, v.Len(), v.Len())
	if !isInline {
		for i := 0; i < len(keys); i++ {
			keys[i] = newPrimaryKey(e.Name(), parentKey[0])
		}
	}

	cols := e.Columns()
	buf.WriteString("INSERT INTO ")
	buf.WriteString(b.db.dialect.GetTable(e.Name()))
	buf.WriteString(" (")
	buf.WriteString(b.db.dialect.Quote(strings.Join(e.Columns(), b.db.dialect.Quote(","))))
	buf.WriteString(") ")
	buf.WriteString("VALUES ")

	for i := 0; i < v.Len(); i++ {
		f := reflect.Indirect(v.Index(i))
		if !f.IsValid() {
			return nil, fmt.Errorf("goloquent: invalid value entity value %v", f)
		}

		vi := reflect.New(f.Type())
		vi.Elem().Set(f)

		fv := mustGetField(vi, e.field(keyFieldName))
		if !fv.IsValid() || fv.Type() != typeOfPtrKey {
			return nil, fmt.Errorf("goloquent: entity %q has no primary key property", f.Type().Name())
		}
		pk := newPrimaryKey(e.Name(), keys[i])
		if isInline {
			kk, isOk := fv.Interface().(*datastore.Key)
			if !isOk {
				return nil, fmt.Errorf("goloquent: entity %q has no primary key property", f.Type().Name())
			}
			pk = newPrimaryKey(e.Name(), kk)
		}
		fv.Set(reflect.ValueOf(pk))

		if x, isOk := vi.Interface().(Saver); isOk {
			if err := x.Save(); err != nil {
				return nil, err
			}
		}
		props, err := SaveStruct(vi.Interface())
		if err != nil {
			return nil, nil
		}

		props[pkColumn] = Property{[]string{pkColumn}, typeOfPtrKey, stringPk(pk)}
		f.Set(vi.Elem())
		if i != 0 {
			buf.WriteString(",")
		}
		vals := make([]interface{}, len(cols), len(cols))
		for j, c := range cols {
			vv, err := props[c].Interface()
			if err != nil {
				return nil, err
			}
			vals[j] = vv
		}

		buf.WriteString("(")
		for j := 1; j <= len(cols); j++ {
			buf.WriteString(variable + ",")
		}
		buf.Truncate(buf.Len() - 1)
		buf.WriteString(")")
		args = append(args, vals...)
	}
	buf.WriteString(";")

	return &Stmt{
		query: buf,
		args:  args,
	}, nil
}

func (b *builder) put(model interface{}, parentKey []*datastore.Key) error {
	e, err := newEntity(model)
	if err != nil {
		return err
	}
	e.setName(b.query.table)
	if e.slice.Elem().Len() <= 0 {
		return nil
	}
	stmt, err := b.putStmt(parentKey, e)
	if err != nil {
		return err
	}
	return b.db.client.ExecStmt(stmt)
}

func (b *builder) upsert(model interface{}, parentKey []*datastore.Key) error {
	e, err := newEntity(model)
	if err != nil {
		return err
	}
	e.setName(b.query.table)
	if e.slice.Elem().Len() <= 0 {
		return nil
	}
	stmt, err := b.putStmt(parentKey, e)
	if err != nil {
		return err
	}
	cols := e.Columns()
	omits := newDictionary(b.query.omits)
	for i, c := range cols {
		if !omits.has(c) {
			continue
		}
		cols = append(cols[:i], cols[i+1:]...)
	}
	buf := new(bytes.Buffer)
	buf.WriteString(stmt.Raw())
	buf.Truncate(buf.Len() - 1)
	if len(cols) > 0 {
		buf.WriteString(" " + b.db.dialect.OnConflictUpdate(e.Name(), cols))
	}
	buf.WriteString(";")
	return b.db.client.ExecStmt(&Stmt{
		query: buf,
		args:  stmt.Args(),
	})
}

func (b *builder) saveMutation(model interface{}) (*Stmt, error) {
	v := reflect.Indirect(reflect.ValueOf(model))
	if v.Len() <= 0 {
		return new(Stmt), nil
	}
	e, err := newEntity(model)
	if err != nil {
		return nil, err
	}
	e.setName(b.query.table)
	buf, args := new(bytes.Buffer), make([]interface{}, 0)
	buf.WriteString("UPDATE ")
	buf.WriteString(b.db.dialect.GetTable(e.Name()))
	buf.WriteString(" SET ")
	f := v.Index(0)
	if x, isOk := f.Interface().(Saver); isOk {
		if err := x.Save(); err != nil {
			return nil, err
		}
	}
	props, err := SaveStruct(f.Interface())
	if err != nil {
		return nil, err
	}

	pk, isOk := props[keyFieldName].Value.(*datastore.Key)
	if !isOk {
		return nil, fmt.Errorf("goloquent: entity %q has no primary key property", f.Type().Name())
	}
	delete(props, keyFieldName)
	if pk == nil || pk.Incomplete() {
		return nil, fmt.Errorf("goloquent: invalid key value, %v", pk)
	}

	omits := newDictionary(b.query.omits)
	j := int(1)
	for k, p := range props {
		if omits.has(k) {
			continue
		}
		it, err := p.Interface()
		if err != nil {
			return nil, err
		}
		buf.WriteString(fmt.Sprintf("%s = %s,", b.db.dialect.Quote(k), variable))
		args = append(args, it)
		j++
	}
	buf.Truncate(buf.Len() - 1)
	buf.WriteString(fmt.Sprintf(" WHERE %s = %s;", b.db.dialect.Quote(pkColumn), variable))
	args = append(args, stringPk(pk))

	return &Stmt{
		query: buf,
		args:  args,
	}, nil
}

func (b *builder) save(model interface{}) error {
	v := reflect.ValueOf(model)
	if !v.IsValid() {
		return errors.New("goloquent: invalid entity to save")
	}
	vi := reflect.MakeSlice(reflect.SliceOf(v.Type()), 1, 1)
	vi.Index(0).Set(v)
	vv := reflect.New(vi.Type())
	vv.Elem().Set(vi)
	stmt, err := b.saveMutation(vv.Interface())
	if err != nil {
		return err
	}
	if err := b.db.client.ExecStmt(stmt); err != nil {
		return err
	}
	v.Elem().Set(vi.Index(0).Elem())
	return nil
}

func (b *builder) updateWithMap(v reflect.Value) (*Stmt, error) {
	buf, args := new(strings.Builder), make([]interface{}, 0)
	for i, k := range v.MapKeys() {
		if i > 0 {
			buf.WriteString(",")
		}
		vv := v.MapIndex(k)
		if k.Kind() != reflect.String {
			return nil, fmt.Errorf("goloquent: invalid map key data type, %q", k.Kind())
		}
		kk := k.String()
		if kk == keyFieldName {
			return nil, fmt.Errorf("goloquent: update __key__ is not allow")
		}
		buf.WriteString(b.db.dialect.Quote(kk))
		buf.WriteString(" = ")
		buf.WriteString(variable)
		v, err := normalizeValue(vv.Interface())
		if err != nil {
			return nil, err
		}
		it, err := interfaceToValue(v)
		if err != nil {
			return nil, err
		}
		vi, err := marshal(it)
		if err != nil {
			return nil, err
		}
		args = append(args, vi)
	}

	return &Stmt{
		query: buf,
		args:  args,
	}, nil
}

func (b *builder) updateWithStruct(model interface{}) (*Stmt, error) {
	vi := reflect.Indirect(reflect.ValueOf(model))
	vv := reflect.New(vi.Type())
	vv.Elem().Set(vi)
	if err := checkSinglePtr(vv.Interface()); err != nil {
		return nil, err
	}
	cols := newDictionary(b.query.projection)
	buf, args := new(bytes.Buffer), make([]interface{}, 0)
	props, err := SaveStruct(vv.Interface())
	if err != nil {
		return nil, err
	}
	for _, p := range props {
		name := p.Name()
		if name == keyFieldName || (!cols.has(name) && p.isZero()) {
			continue
		}
		it, err := p.Interface()
		if err != nil {
			return nil, err
		}
		buf.WriteString(b.db.dialect.Quote(p.Name()))
		buf.WriteString(" = ")
		buf.WriteString(variable)
		buf.WriteString(",")
		args = append(args, it)
	}
	buf.Truncate(buf.Len() - 1)
	return &Stmt{
		query: buf,
		args:  args,
	}, nil
}

func (b *builder) updateMulti(v interface{}) error {
	vi := reflect.Indirect(reflect.ValueOf(v))
	table := b.query.table
	if table == "" {
		table = vi.Type().Name()
	}
	if table == "" {
		return fmt.Errorf("goloquent: missing table name")
	}
	buf, args := new(bytes.Buffer), make([]interface{}, 0)
	buf.WriteString("UPDATE ")
	buf.WriteString(b.db.dialect.GetTable(table))
	buf.WriteString(" SET ")
	switch vi.Type().Kind() {
	case reflect.Map:
		if vi.IsNil() || vi.Len() == 0 {
			return nil
		}
		stmt, err := b.updateWithMap(vi)
		if err != nil {
			return err
		}
		buf.WriteString(stmt.Raw())
		args = append(args, stmt.Args()...)
	case reflect.Struct:
		stmt, err := b.updateWithStruct(v)
		if err != nil {
			return err
		}
		buf.WriteString(" ")
		buf.WriteString(stmt.Raw())
		args = append(args, stmt.Args()...)
	default:
		return fmt.Errorf("goloquent: unsupported data type %v on `Update`", vi.Type())
	}

	stmt, err := b.buildStmt(b.query)
	if err != nil {
		return err
	}
	if b.query.limit > 0 && !b.db.dialect.UpdateWithLimit() {
		buf.WriteString(fmt.Sprintf(" WHERE %s IN (",
			b.db.dialect.Quote(pkColumn)))
		buf.WriteString(fmt.Sprintf("SELECT %s FROM %s",
			b.db.dialect.Quote(pkColumn),
			b.db.dialect.GetTable(table)))
		buf.WriteString(stmt.Raw())
		buf.WriteString(")")
	} else {
		buf.WriteString(stmt.Raw())
	}
	buf.WriteString(";")
	return b.db.client.ExecStmt(&Stmt{
		query: buf,
		args:  append(args, stmt.Args()...),
	})
}

func (b *builder) concatKeys(e *entity) (*Stmt, error) {
	v := e.slice.Elem()
	buf, args := new(strings.Builder), make([]interface{}, 0)
	buf.WriteString("(")
	for i := 0; i < v.Len(); i++ {
		f := v.Index(i)
		if i != 0 {
			buf.WriteString(",")
		}
		kk, isOk := mustGetField(f, e.field(keyFieldName)).Interface().(*datastore.Key)
		if !isOk {
			return nil, fmt.Errorf("goloquent: entity %q has no primary key property", f.Type().Name())
		}
		if kk.Incomplete() {
			return nil, fmt.Errorf("goloquent: entity %q has incomplete key", f.Type().Name())
		}
		buf.WriteString(variable)
		args = append(args, stringPk(kk))
	}
	buf.WriteString(")")
	return &Stmt{
		query: buf,
		args:  args,
	}, nil
}

func (b *builder) softDeleteStmt(e *entity) (*Stmt, error) {
	buf, args := new(strings.Builder), make([]interface{}, 0)
	buf.WriteString("UPDATE ")
	buf.WriteString(b.db.dialect.GetTable(e.Name()))
	buf.WriteString(" SET ")
	buf.WriteString(b.db.dialect.Quote(softDeleteColumn))
	buf.WriteString(" = ")
	buf.WriteString(variable)
	buf.WriteString(" WHERE ")
	buf.WriteString(b.db.dialect.Quote(pkColumn))
	buf.WriteString(" IN ")
	args = append(args, time.Now().UTC().Format("2006-01-02 15:04:05"))
	stmt, err := b.concatKeys(e)
	if err != nil {
		return nil, err
	}
	buf.WriteString(stmt.Raw())
	buf.WriteString(";")
	return &Stmt{
		query: buf,
		args:  append(args, stmt.Args()...),
	}, nil
}

func (b *builder) deleteStmt(e *entity, isSoftDelete bool) (*Stmt, error) {
	buf, args := new(strings.Builder), make([]interface{}, 0)
	if isSoftDelete && e.hasSoftDelete() {
		return b.softDeleteStmt(e)
	}
	buf.WriteString("DELETE FROM ")
	buf.WriteString(b.db.dialect.GetTable(e.Name()))
	buf.WriteString(" WHERE ")
	buf.WriteString(b.db.dialect.Quote(pkColumn))
	buf.WriteString(" IN ")
	stmt, err := b.concatKeys(e)
	if err != nil {
		return nil, err
	}
	buf.WriteString(stmt.Raw())
	buf.WriteString(";")
	return &Stmt{
		query: buf,
		args:  append(args, stmt.Args()...),
	}, nil
}

func (b *builder) delete(model interface{}, isSoftDelete bool) error {
	e, err := newEntity(model)
	if err != nil {
		return err
	}
	e.setName(b.query.table)
	stmt, err := b.deleteStmt(e, isSoftDelete)
	if err != nil {
		return err
	}
	return b.db.client.ExecStmt(stmt)
}

func (b *builder) deleteByQuery() error {
	query := b.query
	stmt, err := b.buildStmt(query)
	if err != nil {
		return err
	}
	buf := new(strings.Builder)
	buf.WriteString("DELETE FROM ")
	buf.WriteString(b.db.dialect.GetTable(query.table))
	buf.WriteString(stmt.Raw())
	buf.WriteString(";")
	return b.db.client.ExecStmt(&Stmt{
		query: buf,
		args:  stmt.args,
	})
}

func (b *builder) truncate(tables ...string) error {
	for _, name := range tables {
		buf := new(strings.Builder)
		buf.WriteString("TRUNCATE TABLE ")
		buf.WriteString(b.db.dialect.GetTable(name))
		buf.WriteString(";")
		if err := b.db.client.ExecStmt(&Stmt{
			query: buf,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (b *builder) scan(dest ...interface{}) error {
	query := b.query
	table := query.table
	buf := new(bytes.Buffer)
	buf.WriteString(b.buildSelect(query).Raw())
	buf.WriteString(" FROM ")
	buf.WriteString(b.db.dialect.GetTable(table))
	stmt, err := b.buildStmt(b.query)
	if err != nil {
		return err
	}
	buf.WriteString(stmt.Raw())
	buf.WriteString(";")
	if err := b.db.client.QueryRowStmt(&Stmt{
		query: buf,
		args:  stmt.Args(),
	}).Scan(dest...); err != nil {
		return fmt.Errorf("goloquent: %v", err)
	}
	return nil
}

func (b *builder) runInTransaction(cb TransactionHandler) error {
	conn, isOk := b.db.client.sqlCommon.(*sql.DB)
	if !isOk {
		return fmt.Errorf("goloquent: unable to initiate transaction")
	}
	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("goloquent: unable to begin transaction, %v", err)
	}
	db := b.db.clone()
	db.client.sqlCommon = tx
	defer func() {
		if r := recover(); r != nil {
			defer tx.Rollback()
		}
	}()
	defer tx.Rollback()
	if err := cb(db); err != nil {
		return err
	}
	return tx.Commit()
}

func sha1Sign(s *Stmt) string {
	h, rgx := sha1.New(), regexp.MustCompile(`(?i)FROM.+?(LIMIT)`)
	bb := bytes.TrimSpace(bytes.TrimLeft(bytes.TrimRight(rgx.Find([]byte(s.String())), "LIMIT"), "FROM"))
	h.Write(bb)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func interfaceToKeyString(it interface{}) (interface{}, error) {
	var v interface{}
	switch vi := it.(type) {
	case nil:
		v = vi
	case *datastore.Key:
		v = stringPk(vi)
	case string:
		v = vi
	case []byte:
		v = string(vi)
	case []*datastore.Key:
		arr := make([]interface{}, 0)
		for _, kk := range vi {
			arr = append(arr, stringPk(kk))
		}
		v = arr
	case []interface{}:
		arr := make([]interface{}, 0)
		for _, kk := range vi {
			k, err := interfaceToKeyString(kk)
			if err != nil {
				return nil, err
			}
			arr = append(arr, k)
		}
		v = arr
	default:
		return nil, fmt.Errorf("goloquent: primary key has invalid data type %v", reflect.TypeOf(vi))
	}
	return v, nil
}
