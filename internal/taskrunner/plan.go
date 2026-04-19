package taskrunner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// runPlan collects emitted tasks via the project's Plan callback, writes
// them as JSONL, and creates the items table via DuckDB's
// read_json_auto (so column types are inferred from the struct). A
// matching row per item is seeded into tasks.
//
// If items already has rows (resume), this function is not called.
func (r *Runner[T]) runPlan(ctx context.Context) error {
	tmp, err := os.CreateTemp("", "taskrunner-plan-*.jsonl")
	if err != nil {
		return fmt.Errorf("taskrunner: plan tmp: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	w := bufio.NewWriter(tmp)
	enc := json.NewEncoder(w)

	var count int
	ids := make(map[string]struct{})
	emit := func(t T) {
		buf, encErr := json.Marshal(t)
		if encErr != nil {
			// Defer to plan-level error via a sentinel written after the
			// loop; simplest path is to record the first error and keep
			// going so the user gets one clean message.
			if r.planErr == nil {
				r.planErr = fmt.Errorf("taskrunner: marshal plan row: %w", encErr)
			}
			return
		}
		var probe struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(buf, &probe); err != nil || probe.ID == "" {
			if r.planErr == nil {
				r.planErr = fmt.Errorf("taskrunner: emitted row missing 'id' field: %s", string(buf))
			}
			return
		}
		if _, dup := ids[probe.ID]; dup {
			if r.planErr == nil {
				r.planErr = fmt.Errorf("taskrunner: duplicate id in plan: %q", probe.ID)
			}
			return
		}
		ids[probe.ID] = struct{}{}
		if err := enc.Encode(json.RawMessage(buf)); err != nil {
			if r.planErr == nil {
				r.planErr = fmt.Errorf("taskrunner: write plan row: %w", err)
			}
			return
		}
		count++
	}

	if err := r.cfg.Plan(ctx, emit); err != nil {
		tmp.Close()
		return fmt.Errorf("taskrunner: plan: %w", err)
	}
	if r.planErr != nil {
		tmp.Close()
		return r.planErr
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return fmt.Errorf("taskrunner: flush plan: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("taskrunner: close plan tmp: %w", err)
	}

	if count == 0 {
		// Empty plan — still create items so items_empty flips to false
		// and we don't re-plan on the next invocation.
		if _, err := r.db.ExecContext(ctx,
			`CREATE TABLE IF NOT EXISTS items (id TEXT PRIMARY KEY)`); err != nil {
			return fmt.Errorf("taskrunner: create empty items: %w", err)
		}
		return nil
	}

	// CREATE TABLE AS from the JSONL. DuckDB infers columns, including
	// id TEXT. We rely on read_json_auto reading the single temp file.
	createSQL := fmt.Sprintf(
		`CREATE TABLE items AS SELECT * FROM read_json_auto(%s)`,
		sqlQuote(tmpPath),
	)
	if _, err := r.db.ExecContext(ctx, createSQL); err != nil {
		return fmt.Errorf("taskrunner: create items from plan: %w", err)
	}

	// Enforce id uniqueness explicitly — CREATE TABLE AS doesn't set a PK.
	var distinctCount, totalCount int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT id), COUNT(*) FROM items`).
		Scan(&distinctCount, &totalCount); err != nil {
		return fmt.Errorf("taskrunner: verify ids: %w", err)
	}
	if distinctCount != totalCount {
		return fmt.Errorf("taskrunner: duplicate ids in items (%d distinct of %d)",
			distinctCount, totalCount)
	}

	// Seed tasks from items.
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO tasks (id) SELECT id FROM items`); err != nil {
		return fmt.Errorf("taskrunner: seed tasks: %w", err)
	}

	return nil
}

// sqlQuote returns s as a DuckDB string literal (single-quoted, '' escaped).
func sqlQuote(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'', '\'')
		} else {
			out = append(out, s[i])
		}
	}
	out = append(out, '\'')
	return string(out)
}

