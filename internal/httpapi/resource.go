package httpapi

import (
	"errors"
	"time"

	"github.com/smusali/sandbox-api/internal/postgres"
)

type pageEnd struct {
	count int
	next  *string
}

func resourceFailure(err error) error {
	if errors.Is(err, postgres.ErrNotFound) {
		return NotFound()
	}
	return persistenceFailure(err)
}

func persistenceFailure(err error) error {
	if postgres.Unavailable(err) {
		return ServiceUnavailable()
	}
	return err
}

func queryPage(span Span) postgres.Page {
	page := postgres.Page{Limit: span.Limit()}
	if !span.HasAfter() {
		return page
	}
	after := span.After()
	page.After = &postgres.Position{CreatedAt: after.CreatedAt(), ID: after.ID()}
	return page
}

func endPage(collection Collection, scope ID, fetched, limit int, at func(int) (time.Time, ID)) (pageEnd, error) {
	count, more := Trim(fetched, limit)
	if !more {
		return pageEnd{count: count}, nil
	}
	created, id := at(count - 1)
	next, err := NextCursor(collection, scope, created, id, true)
	if err != nil {
		return pageEnd{}, err
	}
	return pageEnd{count: count, next: next}, nil
}
