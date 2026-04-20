package taskrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// itemSchema is the column shape derived from the factory's sample
// Task. Columns are ordered by struct field order so CREATE TABLE is
// deterministic.
type itemSchema struct {
	columns    []itemColumn
	idIdx      int          // index into columns of the `id` field
	schemaType reflect.Type // the sample's concrete type (pointer or value); plan-loop validates each emit against this
}

type itemColumn struct {
	name    string       // JSON tag name (= SQLite column name)
	goType  reflect.Type // field type on the struct
	sqlType string       // SQLite affinity: TEXT / INTEGER / REAL / BLOB
	isJSON  bool         // true for complex Go types stored as JSON TEXT
}

// inferItemSchema walks the sample Task's exported fields and derives a
// SQLite column list. Fields tagged `json:"-"` are skipped; struct /
// slice / map / array / interface fields become JSON TEXT. One column
// MUST be tagged `id`; it becomes items' PRIMARY KEY.
func inferItemSchema(sample any) (itemSchema, error) {
	if sample == nil {
		return itemSchema{}, fmt.Errorf("taskrunner: factory.Hydrate returned nil")
	}
	declaredType := reflect.TypeOf(sample)
	t := declaredType
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return itemSchema{}, fmt.Errorf("taskrunner: Task must be a struct (or pointer to struct), got %s", t.Kind())
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
		return itemSchema{}, fmt.Errorf("taskrunner: Task type %s has no `json:\"id\"` field", t.Name())
	}
	return itemSchema{columns: cols, idIdx: idIdx, schemaType: declaredType}, nil
}

// jsonFieldName extracts the JSON column name from a struct field's
// `json` tag.
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

// sqliteTypeFor maps a Go type to a SQLite column affinity.
func sqliteTypeFor(t reflect.Type) (string, bool, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
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
		if t.Elem().Kind() == reflect.Uint8 {
			return "BLOB", false, nil
		}
		return "TEXT", true, nil
	case reflect.Map, reflect.Struct, reflect.Array:
		return "TEXT", true, nil
	case reflect.Interface:
		return "TEXT", true, nil
	}
	return "", false, fmt.Errorf("unsupported field kind %s", t.Kind())
}

// runPlan builds items (if absent) and streams emitted rows through a
// prepared INSERT. Consumes the factory's List iterator; each yielded
// (task, nil) is type-checked against the cached schema, bound, and
// INSERTed. A yielded error aborts planning; a bind or INSERT failure
// stops the iterator via yield-returning-false.
//
// Called only when itemsEmpty returned true.
func (r *Runner) runPlan(ctx context.Context) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("taskrunner: plan begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// CREATE TABLE items — but only if absent. If a previous
	// plan-with-zero-emits left behind a (id TEXT PRIMARY KEY) stub,
	// drop it first.
	var existingCols int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('items')`).Scan(&existingCols); err != nil {
		return fmt.Errorf("taskrunner: probe items cols: %w", err)
	}
	if existingCols > 0 && existingCols < len(r.schema.columns) {
		if _, err := tx.ExecContext(ctx, `DROP TABLE items`); err != nil {
			return fmt.Errorf("taskrunner: drop stub items: %w", err)
		}
		existingCols = 0
	}
	if existingCols == 0 {
		if _, err := tx.ExecContext(ctx, r.schema.createTableSQL()); err != nil {
			return fmt.Errorf("taskrunner: create items: %w", err)
		}
	}

	stmt, err := tx.PrepareContext(ctx, r.schema.insertSQL())
	if err != nil {
		return fmt.Errorf("taskrunner: prepare insert items: %w", err)
	}
	defer stmt.Close()

	ids := make(map[string]struct{})
	r.planErr = nil

	seq := r.cfg.Factory.List(ctx)
	for task, yieldErr := range seq {
		if yieldErr != nil {
			r.planErr = fmt.Errorf("taskrunner: plan: %w", yieldErr)
			break
		}
		if reflect.TypeOf(task) != r.schema.schemaType {
			r.planErr = fmt.Errorf("taskrunner: plan yielded wrong type: got %T, want %s", task, r.schema.schemaType)
			break
		}
		row, idVal, encErr := r.schema.bind(task)
		if encErr != nil {
			r.planErr = fmt.Errorf("taskrunner: bind plan row: %w", encErr)
			break
		}
		if idVal == "" {
			r.planErr = fmt.Errorf("taskrunner: emitted row has empty id")
			break
		}
		if _, dup := ids[idVal]; dup {
			r.planErr = fmt.Errorf("taskrunner: duplicate id in plan: %q", idVal)
			break
		}
		ids[idVal] = struct{}{}
		if _, err := stmt.ExecContext(ctx, row...); err != nil {
			r.planErr = fmt.Errorf("taskrunner: insert items row %q: %w", idVal, err)
			break
		}
	}
	if r.planErr != nil {
		return r.planErr
	}

	// Seed tasks from items. INSERT OR IGNORE for idempotency.
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO tasks (id) SELECT id FROM items`); err != nil {
		return fmt.Errorf("taskrunner: seed tasks: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("taskrunner: plan commit: %w", err)
	}
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

// insertSQL returns `INSERT INTO items(...) VALUES(?,?,...)`.
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

// quoteIdent returns a SQLite-safe double-quoted identifier.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
