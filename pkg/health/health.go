package health

import (
	"fmt"
	"net/http"

	"rwct-agent/pkg/telemetry"
)

func StartServer(port int) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: telemetry.WrapHTTPHandler(mux, "health-http"),
	}
	go func() {
		_ = srv.ListenAndServe()
	}()
	return srv
}
