// Copyright 2017 The Nakama Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"database/sql"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"nakama/pkg/social"

	"github.com/dgrijalva/jwt-go"
	"github.com/gogo/protobuf/proto"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/satori/go.uuid"
	"github.com/uber-go/zap"
	"golang.org/x/crypto/bcrypt"
)

const (
	letters                    = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	errorInvalidPayload        = "Invalid payload"
	errorIDNotFound            = "ID not found"
	errorAccessTokenIsRequired = "Access token is required"
	errorCouldNotLogin         = "Could not login"
	errorCouldNotRegister      = "Could not register"
	errorIDAlreadyInUse        = "ID already in use"
)

var (
	invalidCharsRegex = regexp.MustCompilePOSIX("([[:cntrl:]]|[[:space:]])+")
	emailRegex        = regexp.MustCompile("^.+@.+\\..+$")
)

type authenticationService struct {
	logger         zap.Logger
	config         Config
	db             *sql.DB
	registry       *SessionRegistry
	pipeline       *pipeline
	mux            *mux.Router
	hmacSecretByte []byte
	upgrader       *websocket.Upgrader
	socialClient   *social.Client
	random         *rand.Rand
}

// NewAuthenticationService creates a new AuthenticationService
func NewAuthenticationService(logger zap.Logger, config Config, db *sql.DB, registry *SessionRegistry, tracker Tracker, messageRouter MessageRouter) *authenticationService {
	s := social.NewClient(5 * time.Second)
	p := NewPipeline(config, db, s, tracker, messageRouter)
	a := &authenticationService{
		logger:         logger,
		config:         config,
		db:             db,
		registry:       registry,
		pipeline:       p,
		hmacSecretByte: []byte(config.GetSession().EncryptionKey),
		upgrader:       &websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 1024},
		socialClient:   s,
		random:         rand.New(rand.NewSource(time.Now().UnixNano())),
	}

	a.configure()
	return a
}

func (a *authenticationService) configure() {
	a.mux = mux.NewRouter()

	a.mux.HandleFunc("/user/login", func(w http.ResponseWriter, r *http.Request) {
		a.handleAuth(w, r, a.login)
	}).Methods("POST")

	a.mux.HandleFunc("/user/register", func(w http.ResponseWriter, r *http.Request) {
		a.handleAuth(w, r, a.register)
	}).Methods("POST")

	a.mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		uid, auth := a.authenticateToken(token)
		if !auth {
			http.Error(w, "Missing or invalid token", 401)
			return
		}

		conn, err := a.upgrader.Upgrade(w, r, nil)
		if err != nil {
			//http.Error is invoked automatically from within the Upgrade func
			a.logger.Warn("Could not upgrade to websockets", zap.Error(err))
			return
		}

		a.registry.add(uid, conn, a.pipeline.processRequest)
	}).Methods("GET")
}

func (a *authenticationService) StartServer(mlogger zap.Logger) {
	go func() {
		err := http.ListenAndServe(fmt.Sprintf(":%d", a.config.GetPort()), a.mux)
		if err != nil {
			mlogger.Fatal("Client listener failed", zap.Error(err))
		}
	}()
	mlogger.Info("Client", zap.Int("port", a.config.GetPort()))
}

func (a *authenticationService) handleAuth(w http.ResponseWriter, r *http.Request,
	retrieveUserID func(authReq *AuthenticateRequest) ([]byte, string, int)) {

	w.Header().Set("Content-Type", "application/octet-stream")

	username, _, ok := r.BasicAuth()
	if !ok {
		a.sendAuthError(w, "Missing or invalid authentication header", 400, nil)
		return
	} else if username != a.config.GetTransport().ServerKey {
		a.sendAuthError(w, "Invalid server key", 401, nil)
		return
	}

	data, err := ioutil.ReadAll(http.MaxBytesReader(w, r.Body, a.config.GetTransport().MaxMessageSizeBytes))
	if err != nil {
		a.logger.Warn("Could not read body", zap.Error(err))
		a.sendAuthError(w, "Could not read request body", 400, nil)
		return
	}

	authReq := &AuthenticateRequest{}
	err = proto.Unmarshal(data, authReq)
	if err != nil {
		a.logger.Warn("Could not decode body", zap.Error(err))
		a.sendAuthError(w, "Could not decode body", 400, nil)
		return
	}

	userID, errString, errCode := retrieveUserID(authReq)
	if errString != "" {
		a.sendAuthError(w, errString, errCode, authReq)
		return
	}

	uid, _ := uuid.FromBytes(userID)

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"uid": uid.String(),
		"exp": time.Now().UTC().Add(time.Duration(a.config.GetSession().TokenExpiryMs) * time.Millisecond).Unix(),
	})
	signedToken, _ := token.SignedString(a.hmacSecretByte)

	authResponse := &AuthenticateResponse{CollationId: authReq.CollationId, Payload: &AuthenticateResponse_Session_{&AuthenticateResponse_Session{Token: signedToken}}}
	a.sendAuthResponse(w, authResponse)
}

func (a *authenticationService) sendAuthError(w http.ResponseWriter, error string, errorCode int, authRequest *AuthenticateRequest) {
	var collationID string
	if authRequest != nil {
		collationID = authRequest.CollationId
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(errorCode)
	authResponse := &AuthenticateResponse{CollationId: collationID, Payload: &AuthenticateResponse_Error_{&AuthenticateResponse_Error{error, authRequest}}}
	a.sendAuthResponse(w, authResponse)
}

func (a *authenticationService) sendAuthResponse(w http.ResponseWriter, response *AuthenticateResponse) {
	payload, err := proto.Marshal(response)
	if err != nil {
		a.logger.Error("Could not marshall Response to byte[]", zap.Error(err))
		return
	}

	w.Write(payload)
}

func (a *authenticationService) login(authReq *AuthenticateRequest) ([]byte, string, int) {
	// Route to correct login handler
	var loginFunc func(authReq *AuthenticateRequest) ([]byte, int64, string, int)
	switch authReq.Payload.(type) {
	case *AuthenticateRequest_Device:
		loginFunc = a.loginDevice
	case *AuthenticateRequest_Facebook:
		loginFunc = a.loginFacebook
	case *AuthenticateRequest_Google:
		loginFunc = a.loginGoogle
	case *AuthenticateRequest_GameCenter_:
		loginFunc = a.loginGameCenter
	case *AuthenticateRequest_Steam:
		loginFunc = a.loginSteam
	case *AuthenticateRequest_Email_:
		loginFunc = a.loginEmail
	case *AuthenticateRequest_Custom:
		loginFunc = a.loginCustom
	default:
		return nil, errorInvalidPayload, 400
	}

	userID, disabledAt, message, status := loginFunc(authReq)

	if disabledAt != 0 {
		return nil, "ID disabled", 401
	}

	return userID, message, status
}

func (a *authenticationService) loginDevice(authReq *AuthenticateRequest) ([]byte, int64, string, int) {
	deviceID := authReq.GetDevice()
	if deviceID == "" {
		return nil, 0, "Device ID is required", 400
	} else if invalidCharsRegex.MatchString(deviceID) {
		return nil, 0, "Invalid device ID, no spaces or control characters allowed", 400
	} else if len(deviceID) < 10 || len(deviceID) > 64 {
		return nil, 0, "Invalid device ID, must be 10-64 bytes", 400
	}

	var userID []byte
	var disabledAt int64
	err := a.db.QueryRow("SELECT u.id, u.disabled_at FROM users u, user_device ud WHERE ud.id = $1 AND u.id = ud.user_id",
		deviceID).
		Scan(&userID, &disabledAt)
	if err != nil {
		a.logger.Warn(errorCouldNotLogin, zap.Error(err))
		return nil, 0, errorIDNotFound, 401
	}

	return userID, disabledAt, "", 200
}

func (a *authenticationService) loginFacebook(authReq *AuthenticateRequest) ([]byte, int64, string, int) {
	accessToken := authReq.GetFacebook()
	if accessToken == "" {
		return nil, 0, errorAccessTokenIsRequired, 400
	} else if invalidCharsRegex.MatchString(accessToken) {
		return nil, 0, "Invalid Facebook access token, no spaces or control characters allowed", 400
	}

	fbProfile, err := a.socialClient.GetFacebookProfile(accessToken)
	if err != nil {
		a.logger.Warn("Could not get Facebook profile", zap.Error(err))
		return nil, 0, errorCouldNotLogin, 401
	}

	var userID []byte
	var disabledAt int64
	err = a.db.QueryRow("SELECT id, disabled_at FROM users WHERE facebook_id = $1",
		fbProfile.ID).
		Scan(&userID, &disabledAt)
	if err != nil {
		a.logger.Warn("Could not login with Facebook profile", zap.Error(err))
		return nil, 0, errorIDNotFound, 401
	}

	return userID, disabledAt, "", 200
}

func (a *authenticationService) loginGoogle(authReq *AuthenticateRequest) ([]byte, int64, string, int) {
	accessToken := authReq.GetGoogle()
	if accessToken == "" {
		return nil, 0, errorAccessTokenIsRequired, 400
	} else if invalidCharsRegex.MatchString(accessToken) {
		return nil, 0, "Invalid Google access token, no spaces or control characters allowed", 400
	}

	googleProfile, err := a.socialClient.GetGoogleProfile(accessToken)
	if err != nil {
		a.logger.Warn("Could not get Google profile", zap.Error(err))
		return nil, 0, errorCouldNotLogin, 401
	}

	var userID []byte
	var disabledAt int64
	err = a.db.QueryRow("SELECT id, disabled_at FROM users WHERE google_id = $1",
		googleProfile.ID).
		Scan(&userID, &disabledAt)
	if err != nil {
		a.logger.Warn("Could not login with Google profile", zap.Error(err))
		return nil, 0, errorIDNotFound, 401
	}

	return userID, disabledAt, "", 200
}

func (a *authenticationService) loginGameCenter(authReq *AuthenticateRequest) ([]byte, int64, string, int) {
	gc := authReq.GetGameCenter()
	if gc == nil || gc.PlayerId == "" || gc.BundleId == "" || gc.Timestamp == 0 || gc.Salt == "" || gc.Signature == "" || gc.PublicKeyUrl == "" {
		return nil, 0, errorInvalidPayload, 400
	}

	_, err := a.socialClient.CheckGameCenterID(gc.PlayerId, gc.BundleId, gc.Timestamp, gc.Salt, gc.Signature, gc.PublicKeyUrl)
	if err != nil {
		a.logger.Warn("Could not check Game Center profile", zap.Error(err))
		return nil, 0, errorCouldNotLogin, 401
	}

	var userID []byte
	var disabledAt int64
	err = a.db.QueryRow("SELECT id, disabled_at FROM users WHERE gamecenter_id = $1",
		gc.PlayerId).
		Scan(&userID, &disabledAt)
	if err != nil {
		a.logger.Warn("Could not login with Game Center profile", zap.Error(err))
		return nil, 0, errorIDNotFound, 401
	}

	return userID, disabledAt, "", 200
}

func (a *authenticationService) loginSteam(authReq *AuthenticateRequest) ([]byte, int64, string, int) {
	if a.config.GetSocial().Steam.PublisherKey == "" || a.config.GetSocial().Steam.AppID == 0 {
		return nil, 0, "Steam login not available", 401
	}

	ticket := authReq.GetSteam()
	if ticket == "" {
		return nil, 0, "Steam ticket is required", 400
	} else if invalidCharsRegex.MatchString(ticket) {
		return nil, 0, "Invalid Steam ticket, no spaces or control characters allowed", 400
	}

	steamProfile, err := a.socialClient.GetSteamProfile(a.config.GetSocial().Steam.PublisherKey, a.config.GetSocial().Steam.AppID, ticket)
	if err != nil {
		a.logger.Warn("Could not check Steam profile", zap.Error(err))
		return nil, 0, errorCouldNotLogin, 401
	}

	var userID []byte
	var disabledAt int64
	err = a.db.QueryRow("SELECT id, disabled_at FROM users WHERE steam_id = $1",
		strconv.FormatUint(steamProfile.SteamID, 10)).
		Scan(&userID, &disabledAt)
	if err != nil {
		a.logger.Warn("Could not login with Steam profile", zap.Error(err))
		return nil, 0, errorIDNotFound, 401
	}

	return userID, disabledAt, "", 200
}

func (a *authenticationService) loginEmail(authReq *AuthenticateRequest) ([]byte, int64, string, int) {
	email := authReq.GetEmail()
	if email == nil {
		return nil, 0, errorInvalidPayload, 400
	} else if email.Email == "" {
		return nil, 0, "Email address is required", 400
	} else if invalidCharsRegex.MatchString(email.Email) {
		return nil, 0, "Invalid email address, no spaces or control characters allowed", 400
	} else if !emailRegex.MatchString(email.Email) {
		return nil, 0, "Invalid email address format", 400
	} else if len(email.Email) < 10 || len(email.Email) > 255 {
		return nil, 0, "Invalid email address, must be 10-255 bytes", 400
	}

	var userID []byte
	var hashedPassword []byte
	var disabledAt int64
	err := a.db.QueryRow("SELECT id, password, disabled_at FROM users WHERE email = $1",
		email.Email).
		Scan(&userID, &hashedPassword, &disabledAt)
	if err != nil {
		a.logger.Warn(errorCouldNotLogin, zap.Error(err))
		return nil, 0, "Invalid credentials", 401
	}

	err = bcrypt.CompareHashAndPassword(hashedPassword, []byte(email.Password))
	if err != nil {
		a.logger.Warn("Invalid credentials", zap.Error(err))
		return nil, 0, "Invalid credentials", 401
	}

	return userID, disabledAt, "", 200
}

func (a *authenticationService) loginCustom(authReq *AuthenticateRequest) ([]byte, int64, string, int) {
	customID := authReq.GetCustom()
	if customID == "" {
		return nil, 0, "Custom ID is required", 400
	} else if invalidCharsRegex.MatchString(customID) {
		return nil, 0, "Invalid custom ID, no spaces or control characters allowed", 400
	} else if len(customID) < 10 || len(customID) > 64 {
		return nil, 0, "Invalid custom ID, must be 10-64 bytes", 400
	}

	var userID []byte
	var disabledAt int64
	err := a.db.QueryRow("SELECT id, disabled_at FROM users WHERE custom_id = $1",
		customID).
		Scan(&userID, &disabledAt)
	if err != nil {
		a.logger.Warn(errorCouldNotLogin, zap.Error(err))
		return nil, 0, errorIDNotFound, 401
	}

	return userID, disabledAt, "", 200
}

func (a *authenticationService) register(authReq *AuthenticateRequest) ([]byte, string, int) {
	// Route to correct register handler
	var registerFunc func(tx *sql.Tx, authReq *AuthenticateRequest) ([]byte, string, int)

	switch authReq.Payload.(type) {
	case *AuthenticateRequest_Device:
		registerFunc = a.registerDevice
	case *AuthenticateRequest_Facebook:
		registerFunc = a.registerFacebook
	case *AuthenticateRequest_Google:
		registerFunc = a.registerGoogle
	case *AuthenticateRequest_GameCenter_:
		registerFunc = a.registerGameCenter
	case *AuthenticateRequest_Steam:
		registerFunc = a.registerSteam
	case *AuthenticateRequest_Email_:
		registerFunc = a.registerEmail
	case *AuthenticateRequest_Custom:
		registerFunc = a.registerCustom
	default:
		return nil, errorInvalidPayload, 400
	}

	tx, err := a.db.Begin()
	if err != nil {
		a.logger.Warn("Could not register, transaction begin error", zap.Error(err))
		return nil, errorCouldNotRegister, 500
	}

	userID, errorMessage, errorCode := registerFunc(tx, authReq)

	if errorCode != 200 {
		if tx != nil {
			err = tx.Rollback()
			if err != nil {
				a.logger.Error("Could not rollback transaction", zap.Error(err))
			}
		}
		return userID, errorMessage, errorCode
	}

	err = tx.Commit()
	if err != nil {
		a.logger.Error("Could not commit transaction", zap.Error(err))
		return nil, errorCouldNotRegister, 500
	}

	a.logger.Info("Registration complete", zap.String("uid", uuid.FromBytesOrNil(userID).String()))
	return userID, errorMessage, errorCode
}

func (a *authenticationService) addUserEdgeMetadata(tx *sql.Tx, userID []byte, updatedAt int64) error {
	_, err := tx.Exec("INSERT INTO user_edge_metadata VALUES ($1, 0, 0, $2)", userID, updatedAt)
	return err
}

func (a *authenticationService) registerDevice(txn *sql.Tx, authReq *AuthenticateRequest) ([]byte, string, int) {
	deviceID := authReq.GetDevice()
	if deviceID == "" {
		return nil, "Device ID is required", 400
	} else if invalidCharsRegex.MatchString(deviceID) {
		return nil, "Invalid device ID, no spaces or control characters allowed", 400
	} else if len(deviceID) < 10 || len(deviceID) > 64 {
		return nil, "Invalid device ID, must be 10-64 bytes", 400
	}

	updatedAt := nowMs()

	userID := uuid.NewV4().Bytes()
	_, err := txn.Exec(`
INSERT INTO users (id, handle, created_at, updated_at)
SELECT $1 AS id,
			 $2 AS handle,
       $4 AS created_at,
       $4 AS updated_at
WHERE NOT EXISTS
    (SELECT id
     FROM user_device
     WHERE id = $3)`,
		userID, a.generateHandle(), deviceID, updatedAt)
	if err != nil {
		a.logger.Warn("Could not register, query error", zap.Error(err))
		return nil, errorIDAlreadyInUse, 401
	}
	res, err := txn.Exec("INSERT INTO user_device (id, user_id) VALUES ($1, $2)", deviceID, userID)
	if err != nil {
		a.logger.Warn("Could not register, query error", zap.Error(err))
		return nil, errorIDAlreadyInUse, 401
	}
	if count, _ := res.RowsAffected(); count == 0 {
		return nil, errorIDAlreadyInUse, 401
	}

	err = a.addUserEdgeMetadata(txn, userID, updatedAt)
	if err != nil {
		return nil, errorIDAlreadyInUse, 401
	}

	return userID, "", 200
}

func (a *authenticationService) registerFacebook(tx *sql.Tx, authReq *AuthenticateRequest) ([]byte, string, int) {
	accessToken := authReq.GetFacebook()
	if accessToken == "" {
		return nil, errorAccessTokenIsRequired, 400
	} else if invalidCharsRegex.MatchString(accessToken) {
		return nil, "Invalid Facebook access token, no spaces or control characters allowed", 400
	}

	fbProfile, err := a.socialClient.GetFacebookProfile(accessToken)
	if err != nil {
		a.logger.Warn("Could not get Facebook profile", zap.Error(err))
		return nil, errorCouldNotRegister, 401
	}

	updatedAt := nowMs()

	var userID []byte
	err = tx.QueryRow(`
INSERT INTO users (handle, facebook_id, created_at, updated_at)
SELECT $1 AS handle,
	 $2 AS facebook_id,
	 $3 AS created_at,
	 $3 AS updated_at
WHERE NOT EXISTS
(SELECT id
 FROM users
 WHERE facebook_id = $2) RETURNING id`,
		a.generateHandle(), fbProfile.ID, updatedAt).Scan(&userID)

	if err != nil {
		a.logger.Warn("Could not register new Facebook profile", zap.Error(err))
		return nil, errorIDAlreadyInUse, 401
	}

	l := a.logger.With(zap.String("user_id", uuid.FromBytesOrNil(userID).String()))
	a.pipeline.addFacebookFriends(l, userID, accessToken)

	err = a.addUserEdgeMetadata(tx, userID, updatedAt)
	if err != nil {
		return nil, errorIDAlreadyInUse, 401
	}

	return userID, "", 200
}

func (a *authenticationService) registerGoogle(tx *sql.Tx, authReq *AuthenticateRequest) ([]byte, string, int) {
	accessToken := authReq.GetGoogle()
	if accessToken == "" {
		return nil, errorAccessTokenIsRequired, 400
	} else if invalidCharsRegex.MatchString(accessToken) {
		return nil, "Invalid Google access token, no spaces or control characters allowed", 400
	}

	googleProfile, err := a.socialClient.GetGoogleProfile(accessToken)
	if err != nil {
		a.logger.Warn("Could not get Google profile", zap.Error(err))
		return nil, errorCouldNotRegister, 401
	}

	updatedAt := nowMs()
	var userID []byte
	err = tx.QueryRow(`
INSERT INTO users (handle, google_id, created_at, updated_at)
SELECT $1 AS handle,
	 $2 AS google_id,
	 $3 AS created_at,
	 $3 AS updated_at
WHERE NOT EXISTS
(SELECT id
 FROM users
 WHERE google_id = $2) RETURNING id`,
		a.generateHandle(),
		googleProfile.ID,
		updatedAt).
		Scan(&userID)

	if err != nil {
		a.logger.Warn("Could not register new Google profile", zap.Error(err))
		return nil, errorIDAlreadyInUse, 401
	}

	err = a.addUserEdgeMetadata(tx, userID, updatedAt)
	if err != nil {
		return nil, errorIDAlreadyInUse, 401
	}

	return userID, "", 200
}

func (a *authenticationService) registerGameCenter(tx *sql.Tx, authReq *AuthenticateRequest) ([]byte, string, int) {
	gc := authReq.GetGameCenter()
	if gc == nil || gc.PlayerId == "" || gc.BundleId == "" || gc.Timestamp == 0 || gc.Salt == "" || gc.Signature == "" || gc.PublicKeyUrl == "" {
		return nil, errorInvalidPayload, 400
	}

	_, err := a.socialClient.CheckGameCenterID(gc.PlayerId, gc.BundleId, gc.Timestamp, gc.Salt, gc.Signature, gc.PublicKeyUrl)
	if err != nil {
		a.logger.Warn("Could not get Game Center profile", zap.Error(err))
		return nil, errorCouldNotRegister, 401
	}

	updatedAt := nowMs()
	var userID []byte
	err = tx.QueryRow(`
INSERT INTO users (handle, gamecenter_id, created_at, updated_at)
SELECT $1 AS handle,
	 $2 AS gamecenter_id,
	 $3 AS created_at,
	 $3 AS updated_at
WHERE NOT EXISTS
(SELECT id
 FROM users
 WHERE gamecenter_id = $2) RETURNING id`,
		a.generateHandle(),
		gc.PlayerId,
		updatedAt).
		Scan(&userID)

	if err != nil {
		a.logger.Warn("Could not register new Game Center profile", zap.Error(err))
		return nil, errorIDAlreadyInUse, 401
	}

	err = a.addUserEdgeMetadata(tx, userID, updatedAt)
	if err != nil {
		return nil, errorIDAlreadyInUse, 401
	}

	return userID, "", 200
}

func (a *authenticationService) registerSteam(tx *sql.Tx, authReq *AuthenticateRequest) ([]byte, string, int) {
	if a.config.GetSocial().Steam.PublisherKey == "" || a.config.GetSocial().Steam.AppID == 0 {
		return nil, "Steam registration not available", 401
	}

	ticket := authReq.GetSteam()
	if ticket == "" {
		return nil, "Steam ticket is required", 400
	} else if invalidCharsRegex.MatchString(ticket) {
		return nil, "Invalid Steam ticket, no spaces or control characters allowed", 400
	}

	steamProfile, err := a.socialClient.GetSteamProfile(a.config.GetSocial().Steam.PublisherKey, a.config.GetSocial().Steam.AppID, ticket)
	if err != nil {
		a.logger.Warn("Could not get Steam profile", zap.Error(err))
		return nil, errorCouldNotRegister, 401
	}

	updatedAt := nowMs()

	var userID []byte
	err = tx.QueryRow(`
INSERT INTO users (handle, steam_id, created_at, updated_at)
SELECT $1 AS handle,
	 $2 AS steam_id,
	 $3 AS created_at,
	 $3 AS updated_at
WHERE NOT EXISTS
(SELECT id
 FROM users
 WHERE steam_id = $2) RETURNING id`,
		a.generateHandle(),
		strconv.FormatUint(steamProfile.SteamID, 10),
		updatedAt).
		Scan(&userID)

	if err != nil {
		a.logger.Warn("Could not register new Steam profile", zap.Error(err))
		return nil, errorIDAlreadyInUse, 401
	}

	err = a.addUserEdgeMetadata(tx, userID, updatedAt)
	if err != nil {
		return nil, errorIDAlreadyInUse, 401
	}

	return userID, "", 200
}

func (a *authenticationService) registerEmail(tx *sql.Tx, authReq *AuthenticateRequest) ([]byte, string, int) {
	email := authReq.GetEmail()
	if email == nil {
		return nil, errorInvalidPayload, 400
	} else if email.Email == "" {
		return nil, "Email address is required", 400
	} else if invalidCharsRegex.MatchString(email.Email) {
		return nil, "Invalid email address, no spaces or control characters allowed", 400
	} else if len(email.Password) < 8 {
		return nil, "Password must be longer than 8 characters", 400
	} else if !emailRegex.MatchString(email.Email) {
		return nil, "Invalid email address format", 400
	} else if len(email.Email) < 10 || len(email.Email) > 255 {
		return nil, "Invalid email address, must be 10-255 bytes", 400
	}

	hashedPassword, _ := bcrypt.GenerateFromPassword([]byte(email.Password), bcrypt.DefaultCost)

	updatedAt := nowMs()

	var userID []byte
	err := tx.QueryRow(`
INSERT INTO users (handle, email, password, created_at, updated_at)
SELECT $1 AS handle,
	 $2 AS email,
	 $3 AS password,
	 $4 AS created_at,
	 $4 AS updated_at
WHERE NOT EXISTS
(SELECT id
 FROM users
 WHERE email = $2) RETURNING id`,
		a.generateHandle(),
		email.Email,
		hashedPassword,
		updatedAt).
		Scan(&userID)

	if err != nil {
		a.logger.Warn(errorCouldNotRegister, zap.Error(err))
		return nil, "Email already in use", 401
	}

	err = a.addUserEdgeMetadata(tx, userID, updatedAt)
	if err != nil {
		return nil, "Email already in use", 401
	}

	return userID, "", 200
}

func (a *authenticationService) registerCustom(tx *sql.Tx, authReq *AuthenticateRequest) ([]byte, string, int) {
	customID := authReq.GetCustom()
	if customID == "" {
		return nil, "Custom ID is required", 400
	} else if invalidCharsRegex.MatchString(customID) {
		return nil, "Invalid custom ID, no spaces or control characters allowed", 400
	} else if len(customID) < 10 || len(customID) > 64 {
		return nil, "Invalid custom ID, must be 10-64 bytes", 400
	}

	updatedAt := nowMs()

	var userID []byte
	err := tx.QueryRow(`
INSERT INTO users (handle, custom_id, created_at, updated_at)
SELECT $1 AS handle,
	 $2 AS custom_id,
	 $3 AS created_at,
	 $3 AS updated_at
WHERE NOT EXISTS
(SELECT id
 FROM users
 WHERE custom_id = $2) RETURNING id`,
		a.generateHandle(),
		customID,
		updatedAt).
		Scan(&userID)

	if err != nil {
		a.logger.Warn(errorCouldNotRegister, zap.Error(err))
		return nil, errorIDAlreadyInUse, 401
	}

	err = a.addUserEdgeMetadata(tx, userID, updatedAt)
	if err != nil {
		return nil, errorIDAlreadyInUse, 401
	}

	return userID, "", 200
}

func (a *authenticationService) generateHandle() string {
	b := make([]byte, 10)
	for i := range b {
		b[i] = letters[a.random.Intn(len(letters))]
	}
	return string(b)
}

func (a *authenticationService) authenticateToken(tokenString string) (uuid.UUID, bool) {
	if tokenString == "" {
		a.logger.Warn("Token missing")
		return uuid.Nil, false
	}

	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("Unexpected signing method: %v", token.Header["alg"])
		}
		return a.hmacSecretByte, nil
	})

	if err == nil {
		if claims, ok := token.Claims.(jwt.MapClaims); ok && token.Valid {
			uid, uerr := uuid.FromString(claims["uid"].(string))
			if uerr != nil {
				a.logger.Warn("Invalid user ID in token", zap.String("token", tokenString), zap.Error(uerr))
				return uuid.Nil, false
			}
			return uid, true
		}
	}

	a.logger.Warn("Token invalid", zap.String("token", tokenString), zap.Error(err))
	return uuid.Nil, false
}

func (a *authenticationService) Stop() {
	a.registry.stop()
}

func nowMs() int64 {
	return int64(time.Nanosecond) * time.Now().UTC().UnixNano() / int64(time.Millisecond)
}
