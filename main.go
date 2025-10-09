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
	"github.com/kbm-ky/chirpy/internal/auth"
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
	secret := os.Getenv("SECRET")
	polkaKey := os.Getenv("POLKA_KEY")
	apiConfig := apiConfig{
		dbQueries: dbQueries,
		platform:  platform,
		secret:    secret,
		polkaKey:  polkaKey,
	}
	serveMux.Handle("/app/", apiConfig.middlewareMetricsInc(handlerApp("/app", ".")))
	serveMux.HandleFunc("GET /api/healthz", handlerReadiness)
	serveMux.HandleFunc("POST /api/users", apiConfig.handlerUsers)
	serveMux.HandleFunc("PUT /api/users", apiConfig.handlerPutUsers)
	serveMux.HandleFunc("POST /api/chirps", apiConfig.handlerChirps)
	serveMux.HandleFunc("GET /api/chirps", apiConfig.handlerGetChirps)
	serveMux.HandleFunc("GET /api/chirps/{id}", apiConfig.handlerGetChirp)
	serveMux.HandleFunc("DELETE /api/chirps/{id}", apiConfig.handlerDeleteChirp)
	serveMux.HandleFunc("POST /api/login", apiConfig.handlerLogin)
	serveMux.HandleFunc("POST /api/refresh", apiConfig.handlerRefresh)
	serveMux.HandleFunc("POST /api/revoke", apiConfig.handlerRevoke)
	serveMux.HandleFunc("POST /api/polka/webhooks", apiConfig.handlerPolkaWebhook)
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
	secret         string
	polkaKey       string
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
		Email    string `json:"email"`
		Password string `json:"password"`
	}

	var params parameters
	decoder := json.NewDecoder(req.Body)
	if err := decoder.Decode(&params); err != nil {
		log.Printf("in handlerUsers, unable to decode JSON: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if params.Password == "" {
		log.Printf("in handlerUsers, empty password")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	//hash password
	hashed_password, err := auth.HashPassword(params.Password)
	if err != nil {
		log.Printf("in handlerUsers, unable to hash password: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	//write to database
	createUserArgs := database.CreateUserParams{
		Email:          params.Email,
		HashedPassword: hashed_password,
	}
	dbUser, err := a.dbQueries.CreateUser(req.Context(), createUserArgs)
	if err != nil {
		log.Printf("in handlerUsers, unable to add to database: %v", err)
		w.WriteHeader(400)
		return
	}

	// user := User(dbUser)
	user := User{
		ID:          dbUser.ID,
		CreatedAt:   dbUser.CreatedAt,
		UpdatedAt:   dbUser.UpdatedAt,
		Email:       dbUser.Email,
		IsChripyRed: dbUser.IsChirpyRed,
	}
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

func (a *apiConfig) handlerPutUsers(w http.ResponseWriter, req *http.Request) {
	//Check access token
	accessToken, err := auth.GetBearerToken(req.Header)
	if err != nil {
		log.Printf("in handlerPutUsers, unable to get access token: %v", err)
		w.WriteHeader(401)
		return
	}

	//Authenticate
	userID, err := auth.ValidateJWT(accessToken, a.secret)
	if err != nil {
		log.Printf("in handlerPutUsers, uanble to authenticate user: %v", err)
		w.WriteHeader(401)
		return
	}

	//decode request body
	type reqBody struct {
		Password string `json:"password"`
		Email    string `json:"email"`
	}

	var body reqBody
	decoder := json.NewDecoder(req.Body)
	if err := decoder.Decode(&body); err != nil {
		log.Printf("in handlerPutUsers, unable to decode request body: %v", err)
		w.WriteHeader(401)
		return
	}

	//hash password
	hashedPassword, err := auth.HashPassword(body.Password)
	if err != nil {
		log.Printf("in handlerPutUsers, unable to hash password: %v", err)
		w.WriteHeader(401)
		return
	}

	//update
	updateArgs := database.UpdateUserEmailAndPassParams{
		ID:             userID,
		Email:          body.Email,
		HashedPassword: hashedPassword,
	}
	user, err := a.dbQueries.UpdateUserEmailAndPass(req.Context(), updateArgs)
	if err != nil {
		log.Printf("in handlerPutUsers, unable to update email and password: %v", err)
		w.WriteHeader(401)
		return
	}

	//success
	resUser := User{
		ID:          user.ID,
		CreatedAt:   user.CreatedAt,
		UpdatedAt:   user.UpdatedAt,
		Email:       user.Email,
		IsChripyRed: user.IsChirpyRed,
	}
	jsonDat, err := json.Marshal(resUser)
	if err != nil {
		log.Printf("in handlerPutUsers, unable to encode response: %v", err)
		w.WriteHeader(401)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	w.Write(jsonDat)
}

func (a *apiConfig) handlerGetChirps(w http.ResponseWriter, req *http.Request) {
	//check request for author_id
	var dbChirps []database.Chirp
	authorIDStr := req.URL.Query().Get("author_id")
	authorID, err := uuid.Parse(authorIDStr)
	if err != nil {
		// just get all chirps
		dbChirps, err = a.dbQueries.GetAllChirps(req.Context())
		if err != nil {
			log.Printf("in handlerGetChirps, unable to get all chirps: %v", err)
			w.WriteHeader(501)
			return
		}
	} else {
		//get the chirps for only the author
		dbChirps, err = a.dbQueries.GetChirpsByAuthor(req.Context(), authorID)
		if err != nil {
			log.Printf("in handlerGetChirps, unable to get chirps by author: %v", err)
			w.WriteHeader(501)
			return
		}
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

func (a *apiConfig) handlerDeleteChirp(w http.ResponseWriter, req *http.Request) {
	//authenticate
	//Get bearer token
	accessToken, err := auth.GetBearerToken(req.Header)
	if err != nil {
		log.Printf("in handlerDeleteChirp, unable to get bearer token: %v", err)
		w.WriteHeader(401)
		return
	}

	//validate
	userID, err := auth.ValidateJWT(accessToken, a.secret)
	if err != nil {
		log.Printf("in handlerDeleteChirp, unable to validate: %v", err)
		w.WriteHeader(401)
		return
	}

	//Get chirp id
	chirpIDStr := req.PathValue("id")
	if chirpIDStr == "" {
		log.Printf("in handlerDeleteChirp, no chirp id given")
		w.WriteHeader(404)
		return
	}
	chirpID, err := uuid.Parse(chirpIDStr)
	if err != nil {
		log.Printf("in handlerDeleteChirp, could not parse chirp id: %v", err)
		w.WriteHeader(404)
		return
	}

	//Is user the author?
	chirp, err := a.dbQueries.GetChirp(req.Context(), chirpID)
	if err != nil {
		log.Printf("in handlerDeleteChirp, could not get chirp: %v", err)
		w.WriteHeader(404)
		return
	}

	if userID != chirp.UserID {
		log.Printf("in handlerDeleteChirp, user is not the author")
		w.WriteHeader(403)
		return
	}

	//Delete finally
	err = a.dbQueries.DeleteChirp(req.Context(), chirpID)
	if err != nil {
		log.Printf("in handlerDeleteChirp, unable to delete chirp: %v", err)
		w.WriteHeader(404)
		return
	}

	//success finally?
	w.WriteHeader(204)

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

	//Authenticate
	token, err := auth.GetBearerToken(req.Header)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		log.Printf("in handlerChirps, unable to get bearer token: %v", err)
		return
	}

	userID, err := auth.ValidateJWT(token, a.secret)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		log.Printf("in handlerChirps, unable to validate jwt: %v", err)
		return
	}

	// if userID != chirp.UserID {
	// 	w.WriteHeader(http.StatusUnauthorized)
	// 	log.Printf("in handlerChirps, userID mismatch: %s != %s", userID, chirp.UserID)
	// 	return
	// }

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
		Body: chirp.Body,
		// UserID: chirp.UserID,
		UserID: userID,
	}
	dbChirp, err := a.dbQueries.CreateChirp(req.Context(), createChirpParams)
	if err != nil {
		log.Printf("in handlerChirps, unable to create chirp: %v", err)
		log.Printf("chirp: %v", chirp)
		log.Printf("createChirpParams:%v", createChirpParams)
		w.WriteHeader(501)
		return
	}

	response := Chirp(dbChirp)
	jsonDat, err := json.Marshal(response)
	if err != nil {
		log.Printf("in handlerChirps, unable to encode response: %v", err)
		w.WriteHeader(501)
		return
	}

	w.WriteHeader(201)
	w.Write(jsonDat)
}

func (a *apiConfig) handlerGetChirp(w http.ResponseWriter, req *http.Request) {
	idText := req.PathValue("id")
	if idText == "" {
		log.Printf("in handlerGetChirp: idText = %s", idText)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	log.Printf("handlerGetChirp: idText = %s", idText)

	id, err := uuid.Parse(idText)
	if err != nil {
		log.Printf("in handlerGetChirp: %v", err)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	dbChirp, err := a.dbQueries.GetChirp(req.Context(), id)
	if err != nil {
		log.Printf("in handlerChirps, unable to get chirp: %v", err)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	chirp := Chirp(dbChirp)
	jsonDat, err := json.Marshal(chirp)
	if err != nil {
		log.Printf("unable to encode JSON: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(jsonDat)
}

func (a *apiConfig) handlerLogin(w http.ResponseWriter, req *http.Request) {
	//request
	type loginRequest struct {
		Password         string `json:"password"`
		Email            string `json:"email"`
		ExpiresInSeconds int    `json:"expires_in_seconds,omitempty"`
	}

	var loginReq loginRequest
	decoder := json.NewDecoder(req.Body)
	if err := decoder.Decode(&loginReq); err != nil {
		log.Printf("in handlerLogin, unable to decode JSON: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	//query DB
	dbUser, err := a.dbQueries.GetUserByEmail(req.Context(), loginReq.Email)
	if err != nil {
		log.Printf("in handlerLogin, unable to find user by email: %v", err)
		w.WriteHeader(http.StatusUnauthorized)
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("Incorrect email or password"))
		return
	}

	//check password
	match, err := auth.CheckPassword(loginReq.Password, dbUser.HashedPassword)
	if err != nil {
		log.Printf("in handlerLogin, uanble to check password: %v", err)
		w.WriteHeader(http.StatusUnauthorized)
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("Incorrect email or password"))
		return
	}

	if !match {
		w.WriteHeader(http.StatusUnauthorized)
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("Incorrect email or password"))
		return
	}

	//Generate a token
	// expires_in_seconds := 1 * 60 * 60
	// if loginReq.ExpiresInSeconds > 0 && loginReq.ExpiresInSeconds < 60*60 {
	// 	expires_in_seconds = loginReq.ExpiresInSeconds * 60 * 60
	// }

	// duration := time.Duration(expires_in_seconds) * time.Second
	// log.Printf("in handlerLogin, duration: %v", duration)
	token, err := auth.MakeJWT(dbUser.ID, a.secret, 1*time.Hour)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	//Generate Refresh token
	refreshToken, err := auth.MakeRefreshToken()
	if err != nil {
		log.Printf("in handlerLogin, unable to make refresh token: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Add to DB
	refreshTokenArgs := database.CreateRefreshTokenParams{
		Token:  refreshToken,
		UserID: dbUser.ID,
	}
	_, err = a.dbQueries.CreateRefreshToken(req.Context(), refreshTokenArgs)
	if err != nil {
		log.Printf("in handlerLogin, unable to create refresh token: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	//success
	// user := User{
	// 	Email:     dbUser.Email,
	// 	CreatedAt: dbUser.UpdatedAt,
	// 	UpdatedAt: dbUser.UpdatedAt,
	// 	ID:        dbUser.ID,
	// }
	type userReturn struct {
		ID           uuid.UUID `json:"id"`
		CreatedAt    time.Time `json:"created_at"`
		UpdatedAt    time.Time `json:"updated_at"`
		Email        string    `json:"email"`
		Token        string    `json:"token"`
		RefreshToken string    `json:"refresh_token"`
		IsChirpyRed  bool      `json:"is_chirpy_red"`
	}
	user := userReturn{
		ID:           dbUser.ID,
		CreatedAt:    dbUser.CreatedAt,
		UpdatedAt:    dbUser.UpdatedAt,
		Email:        dbUser.Email,
		Token:        token,
		RefreshToken: refreshToken,
		IsChirpyRed:  dbUser.IsChirpyRed,
	}
	jsonDat, err := json.Marshal(&user)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "application/json")
	w.Write(jsonDat)
}

func (a *apiConfig) handlerRefresh(w http.ResponseWriter, req *http.Request) {
	//Check for Refresh Token in headers
	token, err := auth.GetBearerToken(req.Header)
	if err != nil {
		log.Printf("in handlerRefresh, unable to get bearer token: %v", err)
		w.WriteHeader(401)
		return
	}

	//Is it legit?
	dbTokenRecord, err := a.dbQueries.GetRefreshToken(req.Context(), token)
	if err != nil {
		log.Printf("in handlerRefresh, unable to get refresh token: %v", err)
		w.WriteHeader(401)
		return
	}

	//Is it revoked?
	if dbTokenRecord.RevokedAt.Valid {
		log.Printf("in handlerRefresh, revoked refresh token")
		w.WriteHeader(401)
		return
	}

	//Is it expired?
	if dbTokenRecord.ExpiresAt.Before(time.Now()) {
		log.Printf("in handlerRefresh, expired refresh token")
		w.WriteHeader(401)
		return
	}

	//Create new access token
	accessToken, err := auth.MakeJWT(dbTokenRecord.UserID, a.secret, 1*time.Hour)
	if err != nil {
		log.Printf("in handlerRefresh, unable to make jwt access token: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	//respond
	type refreshResponse struct {
		Token string `json:"token"`
	}

	refRes := refreshResponse{
		Token: accessToken,
	}
	jsonDat, err := json.Marshal(refRes)
	if err != nil {
		log.Printf("in handlerRefresh, unable to encode response: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	w.Write(jsonDat)
}

func (a *apiConfig) handlerRevoke(w http.ResponseWriter, req *http.Request) {
	//Check for refresh token in headers
	token, err := auth.GetBearerToken(req.Header)
	if err != nil {
		log.Printf("in handlerRevoke, unable to get bearer token: %v", err)
		w.WriteHeader(401)
		return
	}

	err = a.dbQueries.RevokeRefreshToken(req.Context(), token)
	if err != nil {
		log.Printf("in handlerRevoke, unable to revoke: %v", err)
		w.WriteHeader(401)
		return
	}

	w.WriteHeader(204)
}

func (a *apiConfig) handlerPolkaWebhook(w http.ResponseWriter, req *http.Request) {
	//Authenticate by checking for ApiKey
	apiKey, err := auth.GetAPIKey(req.Header)
	if err != nil {
		log.Printf("in handlerPolkaWebhook, unable to get API Key: %v", err)
		w.WriteHeader(401)
		return
	}

	//compare
	if apiKey != a.polkaKey {
		log.Printf("in handlerPolkaWebhook, api keys do not match")
		w.WriteHeader(401)
		return
	}

	// Decode request JSON
	type reqBody struct {
		Event string `json:"event"`
		Data  struct {
			UserID string `json:"user_id"`
		} `json:"data"`
	}

	var body reqBody
	decoder := json.NewDecoder(req.Body)
	err = decoder.Decode(&body)
	if err != nil {
		log.Printf("in handlerPolkaWebhook, unable to decode req body: %v", err)
		w.WriteHeader(501)
		return
	}

	//Not an event we care about?  Return immediately
	if body.Event != "user.upgraded" {
		w.WriteHeader(204)
		return
	}

	//Get user ID
	userID, err := uuid.Parse(body.Data.UserID)
	if err != nil {
		log.Printf("in handlerPolkaWebhook, unable to parse user ID: %v", err)
		w.WriteHeader(404)
		return
	}

	//Update user in database
	_, err = a.dbQueries.UpgradeUserChirpyRed(req.Context(), userID)
	if err != nil {
		log.Printf("in handlerPolkaWebhook, unable to upgrade user: %v", err)
		w.WriteHeader(404)
		return
	}

	//Success
	w.WriteHeader(204)
}

type User struct {
	ID          uuid.UUID `json:"id"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Email       string    `json:"email"`
	IsChripyRed bool      `json:"is_chirpy_red"`
}

type Chirp struct {
	ID        uuid.UUID `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Body      string    `json:"body"`
	UserID    uuid.UUID `json:"user_id"`
}
