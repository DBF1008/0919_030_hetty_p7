package api

import (
	"context"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2/ast"

	"github.com/dstotijn/hetty/pkg/log"
	"github.com/dstotijn/hetty/pkg/reqid"
)

// auditExtension is a gqlgen extension that writes one structured audit log
// entry per executed GraphQL operation. It records the correlation ID,
// operation type/name and root fields, latency, and any errors, so mutations
// such as ModifyRequest and SendRequest can be traced end to end.
type auditExtension struct {
	logger log.Logger
}

var (
	_ graphql.HandlerExtension    = auditExtension{}
	_ graphql.ResponseInterceptor = auditExtension{}
)

func newAuditExtension(logger log.Logger) auditExtension {
	if logger == nil {
		logger = nopAuditLogger{}
	}

	return auditExtension{logger: logger}
}

func (auditExtension) ExtensionName() string { return "hetty.AuditLog" }

func (auditExtension) Validate(graphql.ExecutableSchema) error { return nil }

func (a auditExtension) InterceptResponse(ctx context.Context, next graphql.ResponseHandler) *graphql.Response {
	start := time.Now()

	res := next(ctx)

	opCtx := graphql.GetOperationContext(ctx)
	reqID, _ := reqid.FromContext(ctx)

	fields := []interface{}{
		"request_id", reqID.String(),
		"latency_ms", time.Since(start).Milliseconds(),
	}

	if opCtx != nil && opCtx.Operation != nil {
		fields = append(fields, "type", string(opCtx.Operation.Operation))

		if opCtx.Operation.Name != "" {
			fields = append(fields, "operation", opCtx.Operation.Name)
		}

		if rootFields := rootFieldNames(opCtx); len(rootFields) > 0 {
			fields = append(fields, "fields", rootFields)
		}
	}

	if res != nil && len(res.Errors) > 0 {
		fields = append(fields, "errors", res.Errors.Error())
		a.logger.Errorw("GraphQL operation failed.", fields...)
	} else {
		a.logger.Infow("GraphQL operation.", fields...)
	}

	return res
}

// rootFieldNames returns the top level selection names (e.g. ["sendRequest"])
// for the current operation.
func rootFieldNames(opCtx *graphql.OperationContext) []string {
	names := make([]string, 0, len(opCtx.Operation.SelectionSet))

	for _, sel := range opCtx.Operation.SelectionSet {
		if field, ok := sel.(*ast.Field); ok {
			names = append(names, field.Name)
		}
	}

	return names
}

// nopAuditLogger keeps the extension usable when no logger is configured.
type nopAuditLogger struct{}

func (nopAuditLogger) Debugw(string, ...interface{}) {}
func (nopAuditLogger) Infow(string, ...interface{})  {}
func (nopAuditLogger) Errorw(string, ...interface{}) {}
