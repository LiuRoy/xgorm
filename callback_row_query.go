package gorm

import (
	"database/sql"
	"context"
	"github.com/aws/aws-xray-sdk-go/xray"
)

// Define callbacks for row query
func init() {
	DefaultCallback.RowQuery().Register("gorm:row_query", rowQueryCallback)
}

type RowQueryResult struct {
	Row *sql.Row
}

type RowsQueryResult struct {
	Rows  *sql.Rows
	Error error
}

// queryCallback used to query data from database
func rowQueryCallback(scope *Scope) {
	if result, ok := scope.InstanceGet("row_query_result"); ok {
		scope.prepareQuerySQL()

		if rowResult, ok := result.(*RowQueryResult); ok {
			xray.Capture(scope.ctx, "xgorm", func(ctx context.Context) error {
				seg := xray.GetSegment(ctx)

				seg.Lock()
				seg.Namespace = "remote"
				seg.GetSQL().SanitizedQuery = scope.SQL
				seg.Unlock()

				rowResult.Row = scope.SQLDB().QueryRow(scope.SQL, scope.SQLVars...)
				return nil
			})
		} else if rowsResult, ok := result.(*RowsQueryResult); ok {
			rowsResult.Error = xray.Capture(scope.ctx, "xgorm", func(ctx context.Context) error {
				seg := xray.GetSegment(ctx)

				seg.Lock()
				seg.Namespace = "remote"
				seg.GetSQL().SanitizedQuery = scope.SQL
				seg.Unlock()

				var err error
				rowsResult.Rows, err = scope.SQLDB().Query(scope.SQL, scope.SQLVars...)
				return err
			})
		}
	}
}
