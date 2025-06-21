package httplog

import (
	"context"
)

const (
	ErrorKey = "error"
)

type ctxKeyLogKVs struct{}

func (c *ctxKeyLogKVs) String() string {
	return "httplog kv context"
}

// SetKVs sets the keys and values on the request log.
func SetKVs(ctx context.Context, KeysAndValues ...any) {
	if ptr, ok := ctx.Value(ctxKeyLogKVs{}).(*[]any); ok && ptr != nil {
		*ptr = append(*ptr, KeysAndValues...)
	}
}

func getKVs(ctx context.Context) []any {
	if ptr, ok := ctx.Value(ctxKeyLogKVs{}).(*[]any); ok && ptr != nil {
		return *ptr
	}

	return nil
}

// SetError sets the error key and value on the request log.
func SetError(ctx context.Context, err error) error {
	if err != nil {
		SetKVs(ctx, ErrorKey, err)
	}

	return err
}
