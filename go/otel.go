package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

const otelServiceName = "isuride"

// エンドポイントやサンプリングはすべて OTEL_* の標準環境変数で設定する。
// OTLP エンドポイントが未設定、または OTEL_SDK_DISABLED=true のときは
// 何も送らず、計装のオーバーヘッドもほぼ無い状態で動作する。
func setupOTel(ctx context.Context) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }

	if disabled, _ := strconv.ParseBool(os.Getenv("OTEL_SDK_DISABLED")); disabled {
		slog.Info("OpenTelemetry is disabled by OTEL_SDK_DISABLED")
		return noop, nil
	}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		slog.Info("OTLP endpoint is not configured; tracing is disabled")
		return noop, nil
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(otelServiceName)),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		// OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES があれば上書きさせる
		resource.WithFromEnv(),
	)
	if err != nil {
		return noop, err
	}

	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return noop, err
	}

	// サンプラーを明示指定しないことで OTEL_TRACES_SAMPLER で制御できるようにしている
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		slog.Error("otel error", "error", err)
	}))

	slog.Info("OpenTelemetry tracing is enabled")
	return tp.Shutdown, nil
}

// otelhttp はルーティング前に走るため span 名が URL パスのままになる。
// chi がルートを解決した後にパターン付きの名前へ書き換える。
func otelRouteTagMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)

		rctx := chi.RouteContext(r.Context())
		if rctx == nil {
			return
		}
		pattern := rctx.RoutePattern()
		if pattern == "" {
			return
		}
		span := trace.SpanFromContext(r.Context())
		span.SetName(r.Method + " " + pattern)
		span.SetAttributes(semconv.HTTPRoute(pattern))
	})
}
