package httplog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-logr/logr"
)

var (
	ErrClientAborted = fmt.Errorf("request aborted: client disconnected before response was sent")
)

func RequestLogger(logger logr.Logger, o *Options) func(http.Handler) http.Handler {
	if o == nil {
		o = &defaultOptions
	}
	if len(o.LogBodyContentTypes) == 0 {
		o.LogBodyContentTypes = defaultOptions.LogBodyContentTypes
	}
	if o.LogBodyMaxLen == 0 {
		o.LogBodyMaxLen = defaultOptions.LogBodyMaxLen
	}
	s := o.Schema
	if s == nil {
		s = SchemaECS
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := logr.NewContext(r.Context(), logger)
			ctx = context.WithValue(ctx, ctxKeyLogKVs{}, &[]any{})
			logger = logger.V(o.Visibility)

			logReqBody := o.LogRequestBody != nil && o.LogRequestBody(r)
			logRespBody := o.LogResponseBody != nil && o.LogResponseBody(r)

			var reqBody bytes.Buffer
			if logReqBody || o.LogExtraAttrs != nil {
				r.Body = io.NopCloser(io.TeeReader(r.Body, &reqBody))
			}

			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			var respBody bytes.Buffer
			if o.LogResponseBody != nil && o.LogResponseBody(r) {
				ww.Tee(&respBody)
			}

			start := time.Now()

			defer func() {
				var logkvs []any

				if rec := recover(); rec != nil {
					// Return HTTP 500 if recover is enabled and no response status was set.
					if o.RecoverPanics && ww.Status() == 0 && r.Header.Get("Connection") != "Upgrade" {
						ww.WriteHeader(http.StatusInternalServerError)
					}

					if rec == http.ErrAbortHandler || !o.RecoverPanics {
						// Re-panic http.ErrAbortHandler unconditionally, and re-panic other errors if panic recovery is disabled.
						defer panic(rec)
					}

					logkvs = appendKVs(logkvs, s.ErrorMessage, fmt.Sprintf("panic: %v", rec))

					if rec != http.ErrAbortHandler {
						pc := make([]uintptr, 10)   // Capture up to 10 stack frames.
						n := runtime.Callers(3, pc) // Skip 3 frames (this middleware + runtime/panic.go).
						pc = pc[:n]

						// Process panic stack frames to print detailed information.
						frames := runtime.CallersFrames(pc)
						var stackValues []string
						for frame, more := frames.Next(); more; frame, more = frames.Next() {
							if !strings.Contains(frame.File, "runtime/panic.go") {
								stackValues = append(stackValues, fmt.Sprintf("%s:%d", frame.File, frame.Line))
							}
						}
						logkvs = appendKVs(logkvs, s.ErrorStackTrace, stackValues)
					}
				}

				duration := time.Since(start)
				statusCode := ww.Status()
				if statusCode == 0 {
					// If the handler never calls w.WriteHeader(statusCode) explicitly,
					// Go's http package automatically sends HTTP 200 OK to the client.
					statusCode = 200
				}

				// Skip logging if the request is filtered by the Skip function.
				if o.Skip != nil && o.Skip(r, statusCode) {
					return
				}

				var lvl int
				switch {
				case statusCode >= 500:
					lvl = 0 // error
				case statusCode == 429:
					lvl = -2 // info
				case statusCode >= 400:
					lvl = -1 // warning
				case r.Method == "OPTIONS":
					lvl = -3 // debug
				default:
					lvl = -2
				}

				// Skip logging if the message level is below the logger's level or the minimum level specified in options
				if logger.GetV() > lvl {
					return
				}

				logkvs = appendKVs(logkvs,
					s.RequestURL, requestURL(r),
					s.RequestMethod, r.Method,
					s.RequestPath, r.URL.Path,
					s.RequestRemoteIP, r.RemoteAddr,
					s.RequestHost, r.Host,
					s.RequestScheme, scheme(r),
					s.RequestProto, r.Proto,
					s.RequestHeaders, nestKVs(getHeaderKVs(r.Header, o.LogRequestHeaders)),
					s.RequestBytes, r.ContentLength,
					s.RequestUserAgent, r.UserAgent(),
					s.RequestReferer, r.Referer(),
					s.ResponseHeaders, nestKVs(getHeaderKVs(ww.Header(), o.LogResponseHeaders)),
					s.ResponseStatus, statusCode,
					s.ResponseDuration, float64(duration.Milliseconds()),
					s.ResponseBytes, ww.BytesWritten(),
				)

				if err := ctx.Err(); errors.Is(err, context.Canceled) {
					logkvs = appendKVs(logkvs, ErrorKey, ErrClientAborted, s.ErrorType, "ClientAborted")
				}

				if logReqBody || o.LogExtraAttrs != nil {
					// Ensure the request body is fully read if the underlying HTTP handler didn't do so.
					n, _ := io.Copy(io.Discard, r.Body)
					if n > 0 {
						logkvs = appendKVs(logkvs, s.RequestBytesUnread, n)
					}
				}
				if logReqBody {
					logkvs = appendKVs(logkvs, s.RequestBody, logBody(&reqBody, r.Header, o))
				}
				if logRespBody {
					logkvs = appendKVs(logkvs, s.ResponseBody, logBody(&respBody, ww.Header(), o))
				}
				if o.LogExtraAttrs != nil {
					logkvs = appendKVs(logkvs, o.LogExtraAttrs(r, reqBody.String(), statusCode)...)
				}
				logkvs = appendKVs(logkvs, getKVs(ctx)...)

				// Group attributes into nested objects, e.g. for GCP structured logs.
				if s.GroupDelimiter != "" {
					logkvs = groupKVs(logkvs, s.GroupDelimiter)
				}

				msg := fmt.Sprintf("%s %s => HTTP %v (%v)", r.Method, r.URL, statusCode, duration)
				if lvl == 0 { // error
					logger.Error(nil, msg, logkvs...)
				} else {
					logger.Info(msg, logkvs...)
				}
			}()

			next.ServeHTTP(ww, r.WithContext(ctx))
		})
	}
}

func appendKVs(kvpairs []any, newkvs ...any) []any {
	kvpairs = append(kvpairs, newkvs...)
	return kvpairs
}

func groupKVs(kvs []any, delimiter string) []any {
	var result []any
	var nested = map[string][]any{}

	for i, v := range kvs {
		if i%2 == 0 {
			str, ok := v.(string)
			if !ok {
				str = ""
			}
			prefix, key, found := strings.Cut(str, delimiter)
			if !found {
				result = append(result, str)
				continue
			}
			nested[prefix] = append(nested[prefix], key, kvs[i+1])
		}
	}

	for prefix, kvs := range nested {
		result = append(result, prefix, nestKVs(kvs))
	}

	return result
}

func nestKVs(kvs []any) map[string]any {
	m := make(map[string]any, len(kvs)/2+1)
	for i, v := range kvs {
		if i%2 == 0 {
			str, ok := v.(string)
			if !ok {
				str = ""
			}
			m[str] = kvs[i+1]
		}
	}
	return m
}

func getHeaderKVs(header http.Header, headers []string) []any {
	kvs := make([]any, 0, len(headers))
	for _, h := range headers {
		vals := header.Values(h)
		if len(vals) == 1 {
			kvs = append(kvs, h, vals[0])
		} else if len(vals) > 1 {
			kvs = append(kvs, h, vals)
		}
	}
	return kvs
}

func logBody(body *bytes.Buffer, header http.Header, o *Options) string {
	if body.Len() == 0 {
		return ""
	}
	contentType := header.Get("Content-Type")
	for _, whitelisted := range o.LogBodyContentTypes {
		if strings.HasPrefix(contentType, whitelisted) {
			if o.LogBodyMaxLen <= 0 || o.LogBodyMaxLen >= body.Len() {
				return body.String()
			}
			return body.String()[:o.LogBodyMaxLen] + "... [trimmed]"
		}
	}
	return fmt.Sprintf("[body redacted for Content-Type: %s]", contentType)
}
