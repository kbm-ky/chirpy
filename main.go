package main

import (
	"encoding/json"
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
	serveMux.HandleFunc("POST /api/validate_chirp", handlerValidateChirp)
	serveMux.HandleFunc("GET /admin/metrics", apiConfig.handlerMetrics)
	serveMux.HandleFunc("POST /admin/reset", apiConfig.handlerReset)

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

const metricsHtml = `<html>
  <body>
    <h1>Welcome, Chirpy Admin</h1>
    <p>Chirpy has been visited %d times!</p>
  </body>
</html>
`

func (a *apiConfig) handlerMetrics(w http.ResponseWriter, req *http.Request) {
	w.Header().Add("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	output := fmt.Sprintf(metricsHtml, a.fileserverHits.Load())
	w.Write([]byte(output))
}

func (a *apiConfig) handlerReset(w http.ResponseWriter, req *http.Request) {
	w.Header().Add("Content-type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	a.fileserverHits.Swap(0)
}

func handlerValidateChirp(w http.ResponseWriter, req *http.Request) {
	type parameters struct {
		Body string `json:"body"`
	}

	type errorResponse struct {
		Error string `json:"error"`
	}

	type validResponse struct {
		Valid bool `json:"valid"`
	}

	var respData []byte
	var params parameters
	decoder := json.NewDecoder(req.Body)
	if err := decoder.Decode(&params); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("while validating chirp: something went wrong: %v", err)
		errResp := errorResponse{Error: "Something went wrong"}
		respData, err = json.Marshal(errResp)
		if err != nil {
			log.Printf("while validating chirp: while sending error: %v", err)
			respData = []byte{} //zero out again to be safe
		}
	} else {
		if len(params.Body) > 140 {
			w.WriteHeader(400)
			log.Printf("chirp is too long")
			errResp := errorResponse{Error: "Chirp is too long"}
			respData, err = json.Marshal(errResp)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				log.Printf("while responding chirp to long: %v", err)
				respData = []byte{}
			}
		} else {
			log.Printf("chirp length valid")
			w.WriteHeader(http.StatusOK)
			validResp := validResponse{Valid: true}
			respData, err = json.Marshal(validResp)
			if err != nil {
				log.Printf("while responding valid chirp: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				respData = []byte{}
			}
		}
	}
	w.Header().Set("Content-type", "application/json")
	w.Write(respData)
}
