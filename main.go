package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/kbm-ky/chirpy/internal/database"
	_ "github.com/lib/pq"
)

func main() {
	godotenv.Load()
	dbURL := os.Getenv("DB_URL")
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Printf("unable to open database: %v", err)
		os.Exit(1)
	}

	dbQueries := database.New(db)

	fmt.Printf("Starting server...\n")

	serveMux := http.NewServeMux()
	server := http.Server{
		Addr:    ":8080",
		Handler: serveMux,
	}

	platform := os.Getenv("PLATFORM")
	apiConfig := apiConfig{
		dbQueries: dbQueries,
		platform:  platform,
	}
	serveMux.Handle("/app/", apiConfig.middlewareMetricsInc(handlerApp("/app", ".")))
	serveMux.HandleFunc("GET /api/healthz", handlerReadiness)
	// serveMux.HandleFunc("POST /api/validate_chirp", handlerValidateChirp)
	serveMux.HandleFunc("POST /api/users", apiConfig.handlerUsers)
	serveMux.HandleFunc("POST /api/chirps", apiConfig.handlerChirps)
	serveMux.HandleFunc("GET /api/chirps", apiConfig.handlerGetChirps)
	serveMux.HandleFunc("GET /admin/metrics", apiConfig.handlerMetrics)
	serveMux.HandleFunc("POST /admin/reset", apiConfig.handlerReset)

	err = server.ListenAndServe()
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
	dbQueries      *database.Queries
	platform       string
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
	if a.platform != "dev" {
		w.WriteHeader(403)
		return
	}
	err := a.dbQueries.DeleteAllUsers(req.Context())
	if err != nil {
		log.Printf("in handlerReset, unable to delete users: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	a.fileserverHits.Swap(0)
}

func (a *apiConfig) handlerUsers(w http.ResponseWriter, req *http.Request) {
	//get JSON
	type parameters struct {
		Email string `json:"email"`
	}

	var params parameters
	decoder := json.NewDecoder(req.Body)
	if err := decoder.Decode(&params); err != nil {
		log.Printf("in handlerUsers, unable to decode JSON: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	//write to database
	dbUser, err := a.dbQueries.CreateUser(req.Context(), params.Email)
	if err != nil {
		log.Printf("in handlerUsers, unable to add to database: %v", err)
		w.WriteHeader(400)
		return
	}

	user := User(dbUser)
	jsonDat, err := json.Marshal(user)
	if err != nil {
		log.Printf("in handlerUsers, unable to encode JSON response: %v", err)
		w.WriteHeader(400)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	w.Write(jsonDat)
}

func (a *apiConfig) handlerGetChirps(w http.ResponseWriter, req *http.Request) {
	dbChirps, err := a.dbQueries.GetAllChirps(req.Context())
	if err != nil {
		log.Printf("in handlerGetChirps, unable to get all chirps: %v", err)
		w.WriteHeader(501)
		return
	}

	chirps := []Chirp{}
	for _, dbChirp := range dbChirps {
		chirps = append(chirps, Chirp(dbChirp))
	}

	jsonDat, err := json.Marshal(chirps)
	if err != nil {
		log.Printf("in handlerGetChirps, unable to encode JSON: %v", err)
		w.WriteHeader(501)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	w.Write(jsonDat)
}

func (a *apiConfig) handlerChirps(w http.ResponseWriter, req *http.Request) {

	type chirpRequest struct {
		Body   string    `json:"body"`
		UserID uuid.UUID `json:"user_id"`
	}

	type errorResponse struct {
		Error string `json:"error"`
	}

	// Receive from client
	w.Header().Set("Content-Type", "application/json")
	var chirp chirpRequest
	decoder := json.NewDecoder(req.Body)
	if err := decoder.Decode(&chirp); err != nil {
		log.Printf("while validating chirp: something went wrong: %v", err)
		errResp := errorResponse{Error: "Something went wrong"}
		respData, err := json.Marshal(errResp)
		if err != nil {
			log.Printf("while validating chirp: while sending error: %v", err)
			respData = []byte{} //zero out again to be safe
		}
		w.WriteHeader(http.StatusInternalServerError)
		w.Write(respData)
		return
	}

	// Check Length
	if len(chirp.Body) > 140 {
		w.WriteHeader(400)
		log.Printf("chirp is too long")
		errResp := errorResponse{Error: "Chirp is too long"}
		respData, err := json.Marshal(errResp)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			log.Printf("while responding chirp to long: %v", err)
			respData = []byte{}
		}
		w.Write(respData)
		return
	}

	//Check for forbidden words
	badWords := []string{"kerfuffle", "sharbert", "fornax"}
	chirpWords := strings.Fields(chirp.Body)
	cleanedWords := []string{}
	cleaned := false

	for _, word := range chirpWords {
		if slices.Contains(badWords, strings.ToLower(word)) {
			cleanedWords = append(cleanedWords, "****")
			cleaned = true
		} else {
			cleanedWords = append(cleanedWords, word)
		}
	}

	rebuilt := strings.Join(cleanedWords, " ")

	type cleanedResponse struct {
		CleanedBody string `json:"cleaned_body"`
	}

	if cleaned {
		log.Printf("cleaned chirp")
		cleanedBody := cleanedResponse{CleanedBody: rebuilt}
		respData, err := json.Marshal(cleanedBody)
		if err != nil {
			log.Printf("while responding with cleaned chirp: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			respData = []byte{}
		}
		w.WriteHeader(403)
		w.Write(respData)
		return
	}

	//All is well
	log.Printf("chirp valid")

	// call database to save chirp
	createChirpParams := database.CreateChirpParams{
		Body:   chirp.Body,
		UserID: chirp.UserID,
	}
	dbChirp, err := a.dbQueries.CreateChirp(req.Context(), createChirpParams)
	if err != nil {
		log.Printf("in handlerChirps, unable to create chirp: %v", err)
		log.Printf("chirp: %v", chirp)
		log.Printf("createChirpParams:%v", createChirpParams)
		w.WriteHeader(501)
		return
	}

	//response
	type chirpsResponse struct {
		Body   string    `json:"body"`
		UserID uuid.UUID `json:"user_id"`
	}

	response := chirpsResponse{
		Body:   dbChirp.Body,
		UserID: dbChirp.UserID,
	}
	jsonDat, err := json.Marshal(response)
	if err != nil {
		log.Printf("in handlerChirps, unable to encode response: %v", err)
		w.WriteHeader(501)
		return
	}

	w.WriteHeader(201)
	w.Write(jsonDat)
}

// func handlerValidateChirp(w http.ResponseWriter, req *http.Request) {
// 	type parameters struct {
// 		Body string `json:"body"`
// 	}

// 	type errorResponse struct {
// 		Error string `json:"error"`
// 	}

// 	w.Header().Set("Content-type", "application/json")
// 	var respData []byte

// 	// Receive from client
// 	var params parameters
// 	decoder := json.NewDecoder(req.Body)
// 	if err := decoder.Decode(&params); err != nil {
// 		w.WriteHeader(http.StatusInternalServerError)
// 		log.Printf("while validating chirp: something went wrong: %v", err)
// 		errResp := errorResponse{Error: "Something went wrong"}
// 		respData, err = json.Marshal(errResp)
// 		if err != nil {
// 			log.Printf("while validating chirp: while sending error: %v", err)
// 			respData = []byte{} //zero out again to be safe
// 		}
// 		w.Write(respData)
// 		return
// 	}

// 	// Check Length
// 	if len(params.Body) > 140 {
// 		w.WriteHeader(400)
// 		log.Printf("chirp is too long")
// 		errResp := errorResponse{Error: "Chirp is too long"}
// 		respData, err := json.Marshal(errResp)
// 		if err != nil {
// 			w.WriteHeader(http.StatusInternalServerError)
// 			log.Printf("while responding chirp to long: %v", err)
// 			respData = []byte{}
// 		}
// 		w.Write(respData)
// 		return
// 	}

// 	//Check for forbidden words
// 	badWords := []string{"kerfuffle", "sharbert", "fornax"}
// 	chirpWords := strings.Fields(params.Body)
// 	cleanedWords := []string{}
// 	cleaned := false

// 	for _, word := range chirpWords {
// 		if slices.Contains(badWords, strings.ToLower(word)) {
// 			cleanedWords = append(cleanedWords, "****")
// 			cleaned = true
// 		} else {
// 			cleanedWords = append(cleanedWords, word)
// 		}
// 	}

// 	rebuilt := strings.Join(cleanedWords, " ")

// 	type cleanedResponse struct {
// 		CleanedBody string `json:"cleaned_body"`
// 	}

// 	if cleaned {
// 		log.Printf("cleaned chirp")
// 		cleanedBody := cleanedResponse{CleanedBody: rebuilt}
// 		respData, err := json.Marshal(cleanedBody)
// 		if err != nil {
// 			log.Printf("while responding with cleaned chirp: %v", err)
// 			w.WriteHeader(http.StatusInternalServerError)
// 			respData = []byte{}
// 		}
// 		w.Write(respData)
// 		return
// 	}

// 	//All is well
// 	log.Printf("chirp valid")
// 	w.WriteHeader(http.StatusOK)
// 	cleanedResp := cleanedResponse{CleanedBody: params.Body}
// 	respData, err := json.Marshal(cleanedResp)
// 	if err != nil {
// 		log.Printf("while responding valid chirp: %v", err)
// 		w.WriteHeader(http.StatusInternalServerError)
// 		respData = []byte{}
// 		w.Write(respData)
// 		return
// 	}

// 	w.Write(respData)
// }

type User struct {
	ID        uuid.UUID `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Email     string    `json:"email"`
}

type Chirp struct {
	ID        uuid.UUID `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Body      string    `json:"body"`
	UserID    uuid.UUID `json:"user_id"`
}
