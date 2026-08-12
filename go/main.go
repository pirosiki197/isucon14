package main

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/XSAM/otelsql"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

var db *sqlx.DB

func main() {
	ctx := context.Background()

	shutdownOTel, err := setupOTel(ctx)
	if err != nil {
		slog.Error("failed to set up OpenTelemetry", "error", err)
	}

	srv := &http.Server{Addr: ":8080", Handler: setup()}
	go func() {
		slog.Info("Listening on :8080")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("failed to serve", "error", err)
		}
	}()

	// nginx との間は unix ドメインソケットを使う。TCP は matcher が curl で叩くために残す。
	if path := os.Getenv("ISUCON_UNIX_SOCKET"); path != "" {
		ln, err := listenUnix(path)
		if err != nil {
			slog.Error("failed to listen on unix socket", "path", path, "error", err)
		} else {
			go func() {
				slog.Info("Listening on unix socket", "path", path)
				if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					slog.Error("failed to serve on unix socket", "error", err)
				}
			}()
		}
	}

	// systemd の ExecStop が kill -s QUIT を送る。捕まえないと Go の既定動作で
	// スタックダンプを吐いて status 2 で即死し、Restart=on-failure のループになる。
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
	<-sigCtx.Done()
	stop()

	// 終了時に未送信のスパンを吐き出す
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("failed to shutdown server", "error", err)
	}
	if err := shutdownOTel(shutdownCtx); err != nil {
		slog.Error("failed to shutdown OpenTelemetry", "error", err)
	}
}

// 前回終了時のソケットファイルが残っていると bind に失敗するので消してから作る。
// nginx は www-data で動くため、誰でも接続できるパーミッションにする。
func listenUnix(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o777); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func setup() http.Handler {
	host := os.Getenv("ISUCON_DB_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := os.Getenv("ISUCON_DB_PORT")
	if port == "" {
		port = "3306"
	}
	_, err := strconv.Atoi(port)
	if err != nil {
		panic(fmt.Sprintf("failed to convert DB port number from ISUCON_DB_PORT environment variable into int: %v", err))
	}
	user := os.Getenv("ISUCON_DB_USER")
	if user == "" {
		user = "isucon"
	}
	password := os.Getenv("ISUCON_DB_PASSWORD")
	if password == "" {
		password = "isucon"
	}
	dbname := os.Getenv("ISUCON_DB_NAME")
	if dbname == "" {
		dbname = "isuride"
	}

	dbConfig := mysql.NewConfig()
	dbConfig.User = user
	dbConfig.Passwd = password
	dbConfig.Addr = net.JoinHostPort(host, port)
	dbConfig.Net = "tcp"
	dbConfig.DBName = dbname
	dbConfig.ParseTime = true
	// プレースホルダをクライアント側で展開し、クエリごとの PREPARE 往復を省く
	dbConfig.InterpolateParams = true

	sqlDB, err := otelsql.Open("mysql", dbConfig.FormatDSN(),
		otelsql.WithAttributes(
			semconv.DBSystemNameMySQL,
			semconv.ServerAddress(host),
			semconv.DBNamespace(dbname),
		),
		otelsql.WithSpanOptions(otelsql.SpanOptions{
			// クエリ単位のスパンだけ残し、コネクション周りのノイズは落とす
			OmitConnResetSession: true,
			OmitConnectorConnect: true,
			OmitRows:             true,
			DisableErrSkip:       true,
		}),
	)
	if err != nil {
		panic(err)
	}
	db = sqlx.NewDb(sqlDB, "mysql")
	// MaxIdleConns の既定値は 2 で、トランザクションを使わない読み取りだと
	// クエリのたびに接続を張り直してしまう。MySQL の max_connections は 151。
	db.SetMaxOpenConns(100)
	db.SetMaxIdleConns(100)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		panic(err)
	}

	mux := chi.NewRouter()
	mux.Use(otelhttp.NewMiddleware(otelServiceName))
	mux.Use(otelRouteTagMiddleware)
	mux.Use(middleware.Logger)
	mux.Use(middleware.Recoverer)
	mux.HandleFunc("POST /api/initialize", postInitialize)

	// app handlers
	{
		mux.HandleFunc("POST /api/app/users", appPostUsers)

		authedMux := mux.With(appAuthMiddleware)
		authedMux.HandleFunc("POST /api/app/payment-methods", appPostPaymentMethods)
		authedMux.HandleFunc("GET /api/app/rides", appGetRides)
		authedMux.HandleFunc("POST /api/app/rides", appPostRides)
		authedMux.HandleFunc("POST /api/app/rides/estimated-fare", appPostRidesEstimatedFare)
		authedMux.HandleFunc("POST /api/app/rides/{ride_id}/evaluation", appPostRideEvaluatation)
		authedMux.HandleFunc("GET /api/app/notification", appGetNotification)
		authedMux.HandleFunc("GET /api/app/nearby-chairs", appGetNearbyChairs)
	}

	// owner handlers
	{
		mux.HandleFunc("POST /api/owner/owners", ownerPostOwners)

		authedMux := mux.With(ownerAuthMiddleware)
		authedMux.HandleFunc("GET /api/owner/sales", ownerGetSales)
		authedMux.HandleFunc("GET /api/owner/chairs", ownerGetChairs)
	}

	// chair handlers
	{
		mux.HandleFunc("POST /api/chair/chairs", chairPostChairs)

		authedMux := mux.With(chairAuthMiddleware)
		authedMux.HandleFunc("POST /api/chair/activity", chairPostActivity)
		authedMux.HandleFunc("POST /api/chair/coordinate", chairPostCoordinate)
		authedMux.HandleFunc("GET /api/chair/notification", chairGetNotification)
		authedMux.HandleFunc("POST /api/chair/rides/{ride_id}/status", chairPostRideStatus)
	}

	// internal handlers
	{
		mux.HandleFunc("GET /api/internal/matching", internalGetMatching)
	}

	return mux
}

type postInitializeRequest struct {
	PaymentServer string `json:"payment_server"`
}

type postInitializeResponse struct {
	Language string `json:"language"`
}

func postInitialize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req := &postInitializeRequest{}
	if err := bindJSON(r, req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	if out, err := exec.Command("../sql/init.sh").CombinedOutput(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to initialize: %s: %w", string(out), err))
		return
	}

	// init.sh が DB を作り直すので、前回走行の行を捨てる。
	// init.sh の実行中に引き当てられた行も無効なため、実行後に捨てる。
	resetCaches()

	if _, err := db.ExecContext(ctx, "UPDATE settings SET value = ? WHERE name = 'payment_gateway_url'", req.PaymentServer); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, postInitializeResponse{Language: "go"})
}

type Coordinate struct {
	Latitude  int `json:"latitude"`
	Longitude int `json:"longitude"`
}

func bindJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func writeJSON(w http.ResponseWriter, statusCode int, v any) {
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	buf, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(statusCode)
	w.Write(buf)
}

func writeError(w http.ResponseWriter, statusCode int, err error) {
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	w.WriteHeader(statusCode)
	buf, marshalError := json.Marshal(map[string]string{"message": err.Error()})
	if marshalError != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"marshaling error failed"}`))
		return
	}
	w.Write(buf)

	slog.Error("error response wrote", "error", err)
}

func secureRandomStr(b int) string {
	k := make([]byte, b)
	if _, err := crand.Read(k); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", k)
}
