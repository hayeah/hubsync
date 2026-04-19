package taskrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// itemSchema is the column shape derived from T's struct tags. Columns
// are ordered by struct field order, which makes CREATE TABLE output
// deterministic and easy to eyeball.
type itemSchema struct {
	columns []itemColumn
	idIdx   int // index into columns of the `id` field
}

type itemColumn struct {
	name    string       // JSON tag name (= SQLite column name)
	goType  reflect.Type // field type on the struct
	sqlType string       // SQLite affinity: TEXT / INTEGER / REAL
	// isJSON marks complex Go types (structs, slices, maps) that we
	// serialize to JSON and store as TEXT.
	isJSON bool
}

// inferItemSchema walks T's exported fields and derives a SQLite
// column list. Fields with json:"-" are skipped; struct-typed fields
// are stored as JSON text. One column MUST be tagged `id`; it becomes
// the PRIMARY KEY.
func inferItemSchema[T Task]() (itemSchema, error) {
	// reflect.TypeFor[T]() works for both value and pointer T. Callers
	// always pass a pointer type in practice, so we unwrap one level.
	t := reflect.TypeFor[T]()
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return itemSchema{}, fmt.Errorf("taskrunner: T must be a struct (or pointer to struct), got %s", t.Kind())
	}

	var cols []itemColumn
	idIdx := -1
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, keep := jsonFieldName(f)
		if !keep {
			continue
		}
		sqlType, isJSON, err := sqliteTypeFor(f.Type)
		if err != nil {
			return itemSchema{}, fmt.Errorf("taskrunner: field %s: %w", f.Name, err)
		}
		if name == "id" {
			idIdx = len(cols)
		}
		cols = append(cols, itemColumn{
			name:    name,
			goType:  f.Type,
			sqlType: sqlType,
			isJSON:  isJSON,
		})
	}
	if idIdx < 0 {
		return itemSchema{}, fmt.Errorf("taskrunner: T %s has no `json:\"id\"` field", t.Name())
	}
	return itemSchema{columns: cols, idIdx: idIdx}, nil
}

// jsonFieldName extracts the JSON column name from a struct field's
// `json` tag. Returns (name, keep). keep=false for fields tagged "-" or
// otherwise excluded (e.g. transient unexported). Fields without a
// `json` tag are included under their Go field name.
func jsonFieldName(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	if tag == "" {
		return f.Name, true
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		name = f.Name
	}
	return name, true
}

// sqliteTypeFor maps a Go type to a SQLite column affinity. Returns
// (sqlType, isJSON). isJSON signals that values should be
// json.Marshal'd into a string before binding.
func sqliteTypeFor(t reflect.Type) (string, bool, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	// Special case time.Time → TEXT in ISO-8601.
	if t == reflect.TypeOf(time.Time{}) {
		return "TEXT", false, nil
	}
	switch t.Kind() {
	case reflect.String:
		return "TEXT", false, nil
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "INTEGER", false, nil
	case reflect.Float32, reflect.Float64:
		return "REAL", false, nil
	case reflect.Slice:
		// []byte → BLOB; other slices → JSON TEXT.
		if t.Elem().Kind() == reflect.Uint8 {
			return "BLOB", false, nil
		}
		return "TEXT", true, nil
	case reflect.Map, reflect.Struct, reflect.Array:
		return "TEXT", true, nil
	case reflect.Interface:
		// Values land in the DB as whatever concrete type the emitter
		// hands us; binding as JSON is the safe catch-all.
		return "TEXT", true, nil
	}
	return "", false, fmt.Errorf("unsupported field kind %s", t.Kind())
}

// runPlan builds the items table (if it doesn't yet exist) and streams
// emitted rows through a prepared INSERT. Each emitted row is reflected
// against the inferred schema; complex fields are JSON-marshaled before
// binding.
//
// Called only when itemsEmpty returned true.
func (r *Runner[T]) runPlan(ctx context.Context) error {
	schema, err := inferItemSchema[T]()
	if err != nil {
		return err
	}

	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("taskrunner: plan begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// CREATE TABLE items (...) — but only if absent. If a previous
	// plan-with-zero-emits left behind a (id TEXT PRIMARY KEY) stub,
	// drop it first so the next plan gets the full schema.
	var existingCols int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('items')`).Scan(&existingCols); err != nil {
		return fmt.Errorf("taskrunner: probe items cols: %w", err)
	}
	if existingCols > 0 && existingCols < len(schema.columns) {
		if _, err := tx.ExecContext(ctx, `DROP TABLE items`); err != nil {
			return fmt.Errorf("taskrunner: drop stub items: %w", err)
		}
		existingCols = 0
	}
	if existingCols == 0 {
		if _, err := tx.ExecContext(ctx, schema.createTableSQL()); err != nil {
			return fmt.Errorf("taskrunner: create items: %w", err)
		}
	}

	insertSQL := schema.insertSQL()
	stmt, err := tx.PrepareContext(ctx, insertSQL)
	if err != nil {
		return fmt.Errorf("taskrunner: prepare insert items: %w", err)
	}
	defer stmt.Close()

	var count int
	ids := make(map[string]struct{})
	emit := func(t T) {
		if r.planErr != nil {
			return
		}
		row, idVal, encErr := schema.bind(t)
		if encErr != nil {
			r.planErr = fmt.Errorf("taskrunner: bind plan row: %w", encErr)
			return
		}
		if idVal == "" {
			r.planErr = fmt.Errorf("taskrunner: emitted row has empty id")
			return
		}
		if _, dup := ids[idVal]; dup {
			r.planErr = fmt.Errorf("taskrunner: duplicate id in plan: %q", idVal)
			return
		}
		ids[idVal] = struct{}{}
		if _, err := stmt.ExecContext(ctx, row...); err != nil {
			r.planErr = fmt.Errorf("taskrunner: insert items row %q: %w", idVal, err)
			return
		}
		count++
	}

	if err := r.cfg.Plan(ctx, emit); err != nil {
		return fmt.Errorf("taskrunner: plan: %w", err)
	}
	if r.planErr != nil {
		return r.planErr
	}

	// Seed tasks from items. INSERT OR IGNORE so re-runs on a
	// partially-seeded DB stay idempotent.
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO tasks (id) SELECT id FROM items`); err != nil {
		return fmt.Errorf("taskrunner: seed tasks: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("taskrunner: plan commit: %w", err)
	}
	_ = count
	return nil
}

// createTableSQL returns the CREATE TABLE statement for this schema.
func (s itemSchema) createTableSQL() string {
	var b strings.Builder
	b.WriteString("CREATE TABLE items (\n")
	for i, c := range s.columns {
		b.WriteString("  ")
		b.WriteString(quoteIdent(c.name))
		b.WriteByte(' ')
		b.WriteString(c.sqlType)
		if i == s.idIdx {
			b.WriteString(" PRIMARY KEY")
		}
		if i < len(s.columns)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString(")")
	return b.String()
}

// insertSQL returns `INSERT INTO items(...) VALUES(?,?,...)` for the
// schema, bound in column order.
func (s itemSchema) insertSQL() string {
	var cols, vals strings.Builder
	for i, c := range s.columns {
		if i > 0 {
			cols.WriteByte(',')
			vals.WriteByte(',')
		}
		cols.WriteString(quoteIdent(c.name))
		vals.WriteByte('?')
	}
	return fmt.Sprintf("INSERT INTO items(%s) VALUES(%s)",
		cols.String(), vals.String())
}

// bind reflects v against the schema and produces the args slice for
// prepared INSERT, plus the extracted id string.
func (s itemSchema) bind(v any) ([]any, string, error) {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, "", fmt.Errorf("emitted nil pointer")
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, "", fmt.Errorf("emitted non-struct %s", rv.Kind())
	}

	args := make([]any, len(s.columns))
	// Column → struct field lookup by json tag. We can't assume
	// struct field index equals column index because some fields are
	// skipped (json:"-"). Walk the struct and match on tag.
	tagIdx := make(map[string]int, len(s.columns))
	for i, c := range s.columns {
		tagIdx[c.name] = i
	}
	t := rv.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, keep := jsonFieldName(f)
		if !keep {
			continue
		}
		colIdx, ok := tagIdx[name]
		if !ok {
			continue
		}
		fv := rv.Field(i)
		col := s.columns[colIdx]
		if col.isJSON {
			buf, err := json.Marshal(fv.Interface())
			if err != nil {
				return nil, "", fmt.Errorf("marshal %s: %w", name, err)
			}
			args[colIdx] = string(buf)
		} else if col.goType == reflect.TypeOf(time.Time{}) {
			args[colIdx] = fv.Interface().(time.Time).Format(time.RFC3339Nano)
		} else {
			args[colIdx] = fv.Interface()
		}
	}

	idArg := args[s.idIdx]
	idStr, _ := idArg.(string)
	return args, idStr, nil
}

// quoteIdent returns a SQLite-safe double-quoted identifier. Embedded
// double quotes are doubled.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
