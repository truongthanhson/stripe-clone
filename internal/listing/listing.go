// Package listing holds what every list endpoint shares: cursor parameters and page trimming.
package listing

import (
	"fmt"
	"slices"
	"time"
)

// Params pages through objects newest first. StartingAfter and EndingBefore are IDs of objects from
// a previous page; at most one may be set.
type Params struct {
	Limit         int
	StartingAfter string
	EndingBefore  string
}

// Cursor returns the ID to page from, and whether the page goes towards older objects.
func (p Params) Cursor() (id string, older bool) {
	if p.EndingBefore != "" {
		return p.EndingBefore, false
	}
	return p.StartingAfter, true
}

// Keyset appends the cursor condition, order and limit to a query whose rows have created_at and id
// columns. args must already hold the query's other arguments; the cursor's position is appended.
//
// The cursor is a keyset on (created_at, id), not an OFFSET: a page costs the same however deep it is,
// and inserts between requests do not shift items into the next page twice.
func (p Params) Keyset(query string, args []any, cursorCreated time.Time, cursorID string) (string, []any) {
	_, older := p.Cursor()
	if cursorID != "" {
		op := "<"
		if !older {
			op = ">"
		}
		args = append(args, cursorCreated, cursorID)
		query += fmt.Sprintf(` AND (created_at, id) %s ($%d, $%d)`, op, len(args)-1, len(args))
	}
	if older {
		query += ` ORDER BY created_at DESC, id DESC`
	} else {
		query += ` ORDER BY created_at ASC, id ASC`
	}
	// One extra row tells whether there is a next page, without a COUNT(*).
	return query + fmt.Sprintf(` LIMIT %d`, p.Limit+1), args
}

// Trim cuts the extra row Keyset asked for and restores newest-first order. It returns the page and
// whether more objects exist beyond it.
func Trim[T any](p Params, rows []T) ([]T, bool) {
	hasMore := len(rows) > p.Limit
	if hasMore {
		rows = rows[:p.Limit]
	}
	if _, older := p.Cursor(); !older {
		slices.Reverse(rows)
	}
	return rows, hasMore
}
