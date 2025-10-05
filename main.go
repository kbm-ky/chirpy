package main

import (
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
)

func main() {
	fmt.Printf("Starting server...\n")

	serveMux := http.NewServeMux()
	server := http.Server{
		Addr:    ":8080",
		Handler: serveMux,
	}

	apiConfig := apiConfig{}
	serveMux.Handle("/app/", apiConfig.middlewareMetricsInc(handlerApp("/app", ".")))
	serveMux.HandleFunc("GET /api/healthz", handlerReadiness)
	serveMux.HandleFunc("GET /api/metrics", apiConfig.handlerMetrics)
	serveMux.HandleFunc("POST /api/reset", apiConfig.handlerReset)

	err := server.ListenAndServe()
	if err != nil {
		log.Fatalf("unable to listen and serve: %v", err)
	}
}

func handlerReadiness(w http.ResponseWriter, req *http.Request) {
	w.Header().Add("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func handlerApp(strip string, rootPath string) http.Handler {
	log.Printf("hit")
	return http.StripPrefix(strip, http.FileServer(http.Dir(rootPath)))
}

type apiConfig struct {
	fileserverHits atomic.Int32
}

func (a *apiConfig) middlewareMetricsInc(next http.Handler) http.Handler {
	return http.HandlerFunc(
		func(w http.ResponseWriter, req *http.Request) {
			a.fileserverHits.Add(1)
			log.Printf("hit")
			next.ServeHTTP(w, req)
		})
}

func (a *apiConfig) handlerMetrics(w http.ResponseWriter, req *http.Request) {
	w.Header().Add("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	msg := fmt.Sprintf("Hits: %d\n", a.fileserverHits.Load())
	w.Write([]byte(msg))
}

func (a *apiConfig) handlerReset(w http.ResponseWriter, req *http.Request) {
	w.Header().Add("Content-type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	a.fileserverHits.Swap(0)
}
