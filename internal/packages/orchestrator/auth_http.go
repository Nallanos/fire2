package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	authpkg "github/nallanos/fire2/internal/packages/auth"
)

type AuthHandlers struct {
	authSvc *authpkg.Service
}

func NewAuthHandlers(authSvc *authpkg.Service) *AuthHandlers {
	return &AuthHandlers{authSvc: authSvc}
}

func (h *AuthHandlers) Routes() http.Handler {
	r := chi.NewRouter()
	r.Post("/signup", h.signUp)
	r.Post("/login", h.login)
	r.Post("/logout", h.logout)
	return r
}

type authRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type authResponse struct {
	Token string     `json:"token"`
	User  authUserDTO `json:"user"`
}

type authUserDTO struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

func (h *AuthHandlers) signUp(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeAuthRequest(w, r)
	if !ok {
		return
	}
	if !strings.Contains(body.Email, "@") {
		http.Error(w, "valid email is required", http.StatusBadRequest)
		return
	}
	if len(body.Password) < 8 {
		http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
		return
	}

	user, session, err := h.authSvc.SignUp(r.Context(), body.Email, body.Password)
	if err != nil {
		if errors.Is(err, authpkg.ErrEmailTaken) {
			http.Error(w, "email already registered", http.StatusConflict)
			return
		}
		log.Printf("signup failed: %v", err)
		http.Error(w, "signup failed", http.StatusInternalServerError)
		return
	}

	writeAuthResponse(w, http.StatusCreated, user, session)
}

func (h *AuthHandlers) login(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeAuthRequest(w, r)
	if !ok {
		return
	}

	user, session, err := h.authSvc.Login(r.Context(), body.Email, body.Password)
	if err != nil {
		if errors.Is(err, authpkg.ErrInvalidCreds) {
			http.Error(w, "invalid email or password", http.StatusUnauthorized)
			return
		}
		log.Printf("login failed: %v", err)
		http.Error(w, "login failed", http.StatusInternalServerError)
		return
	}

	writeAuthResponse(w, http.StatusOK, user, session)
}

func (h *AuthHandlers) logout(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token != "" {
		if err := h.authSvc.Logout(r.Context(), token); err != nil {
			log.Printf("logout failed: %v", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeAuthRequest(w http.ResponseWriter, r *http.Request) (authRequest, bool) {
	var body authRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return authRequest{}, false
	}
	body.Email = strings.TrimSpace(strings.ToLower(body.Email))
	if body.Email == "" || body.Password == "" {
		http.Error(w, "email and password are required", http.StatusBadRequest)
		return authRequest{}, false
	}
	return body, true
}

func writeAuthResponse(w http.ResponseWriter, status int, user authpkg.User, session authpkg.Session) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(authResponse{
		Token: session.Token,
		User:  authUserDTO{ID: user.ID, Email: user.Email},
	})
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimPrefix(h, prefix)
}

type userContextKey struct{}

// RequireAuth is chi middleware that resolves the request's Bearer token to
// a user and attaches it to the request context, rejecting with 401 when the
// token is missing, unknown, or expired.
func RequireAuth(authSvc *authpkg.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				http.Error(w, "missing bearer token", http.StatusUnauthorized)
				return
			}
			user, err := authSvc.Authenticate(r.Context(), token)
			if err != nil {
				http.Error(w, "invalid or expired session", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userContextKey{}, user)))
		})
	}
}

// UserFromContext returns the user RequireAuth attached to the request
// context. ok is false if called outside a RequireAuth-protected route.
func UserFromContext(ctx context.Context) (user authpkg.User, ok bool) {
	user, ok = ctx.Value(userContextKey{}).(authpkg.User)
	return user, ok
}
