package gateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// Role is a privilege level; a higher role includes the lower ones.
type Role int

const (
	RoleNone     Role = iota
	RoleViewer        // read-only
	RoleOperator      // may operate valves and reset trips
	RoleEngineer      // may also change setpoints and edit/deploy topologies
)

func (r Role) String() string {
	switch r {
	case RoleViewer:
		return "viewer"
	case RoleOperator:
		return "operator"
	case RoleEngineer:
		return "engineer"
	}
	return "none"
}

// ParseRole converts a role name.
func ParseRole(s string) (Role, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "viewer":
		return RoleViewer, true
	case "operator":
		return RoleOperator, true
	case "engineer":
		return RoleEngineer, true
	}
	return RoleNone, false
}

// UserSpec is one account of the users file:
//
//	[{"user": "alice", "role": "operator", "token_sha256": "<hex sha-256 of her token>"}]
//
// Only token hashes are stored; create one with `gateway -hash-token <token>`.
type UserSpec struct {
	User        string `json:"user"`
	Role        string `json:"role"`
	TokenSHA256 string `json:"token_sha256"`
}

// HashToken returns the hex SHA-256 of a token.
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// LoadUsers reads a users file.
func LoadUsers(path string) ([]UserSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var specs []UserSpec
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&specs); err != nil {
		return nil, fmt.Errorf("users file %s: %w", path, err)
	}
	return specs, nil
}

type account struct {
	name string
	role Role
	hash [32]byte
}

// Principal is the authenticated caller of a request.
type Principal struct {
	User          string // empty for the shared token and for unauthenticated (open) mode
	Role          Role
	Authenticated bool // a token was presented and matched
}

// Named reports whether the principal is a personal account.
func (p Principal) Named() bool { return p.User != "" }

// Authenticator validates bearer tokens. Without any configured token or
// account the API is open (everything but control is then available).
type Authenticator struct {
	shared      []byte
	accounts    []account
	controlOpen bool
}

// NewAuthenticator builds an Authenticator. shared is the optional shared
// token of Phase 3 (grants engineer rights but no personal identity, so it can
// never issue control commands). controlOpen enables control without accounts
// ("lab mode"): the user name is then taken from the request and NOT verified.
func NewAuthenticator(shared string, users []UserSpec, controlOpen bool) (*Authenticator, error) {
	a := &Authenticator{shared: []byte(shared), controlOpen: controlOpen}
	seen := map[string]bool{}
	for i, u := range users {
		name := strings.TrimSpace(u.User)
		role, ok := ParseRole(u.Role)
		raw, err := hex.DecodeString(strings.TrimSpace(u.TokenSHA256))
		switch {
		case name == "" || len(name) > 64:
			return nil, fmt.Errorf("user #%d: name is required (max 64 characters)", i+1)
		case seen[name]:
			return nil, fmt.Errorf("user %q is listed twice", name)
		case !ok:
			return nil, fmt.Errorf("user %q: role must be viewer, operator or engineer", name)
		case err != nil || len(raw) != sha256.Size:
			return nil, fmt.Errorf("user %q: token_sha256 must be 64 hex characters", name)
		}
		seen[name] = true
		acc := account{name: name, role: role}
		copy(acc.hash[:], raw)
		a.accounts = append(a.accounts, acc)
	}
	return a, nil
}

// Required reports whether requests must carry a token.
func (a *Authenticator) Required() bool { return len(a.shared) > 0 || len(a.accounts) > 0 }

// ControlOpen reports whether lab mode is on.
func (a *Authenticator) ControlOpen() bool { return a.controlOpen && len(a.accounts) == 0 }

func tokenFrom(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.URL.Query().Get("token") // browsers cannot set headers on a WebSocket handshake
}

// Authenticate identifies the caller. ok is false when a token is required
// but missing or wrong.
func (a *Authenticator) Authenticate(r *http.Request) (Principal, bool) {
	if !a.Required() {
		return Principal{Role: RoleEngineer}, true
	}
	tok := tokenFrom(r)
	if tok == "" {
		return Principal{}, false
	}
	sum := sha256.Sum256([]byte(tok))
	var found *account
	for i := range a.accounts { // no early exit: constant work per request
		if subtle.ConstantTimeCompare(sum[:], a.accounts[i].hash[:]) == 1 {
			found = &a.accounts[i]
		}
	}
	if found != nil {
		return Principal{User: found.name, Role: found.role, Authenticated: true}, true
	}
	if len(a.shared) > 0 && subtle.ConstantTimeCompare([]byte(tok), a.shared) == 1 {
		return Principal{Role: RoleEngineer, Authenticated: true}, true
	}
	return Principal{}, false
}

// ControlAllowed decides whether p may issue control commands at all.
func (a *Authenticator) ControlAllowed(p Principal) (bool, string) {
	switch {
	case p.Named():
		if p.Role < RoleOperator {
			return false, fmt.Sprintf("the %s role may not issue control commands", p.Role)
		}
		return true, ""
	case a.ControlOpen():
		return true, ""
	case len(a.accounts) == 0:
		return false, "control commands are disabled: start the gateway with -users <file> (or -control-open for a lab setup)"
	default:
		return false, "control commands need a personal operator account, not the shared token"
	}
}

type ctxKey struct{}

// PrincipalFrom returns the authenticated caller stored by the auth middleware.
func PrincipalFrom(ctx context.Context) Principal {
	p, _ := ctx.Value(ctxKey{}).(Principal)
	return p
}

// middleware authenticates a request and stores the principal in its context.
func (a *Authenticator) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.Authenticate(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="scada-gateway"`)
			writeError(w, http.StatusUnauthorized, "missing or invalid token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}

// requireRole rejects principals below min. It must run inside middleware.
func requireRole(min Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p := PrincipalFrom(r.Context()); p.Role < min {
				writeError(w, http.StatusForbidden, fmt.Sprintf("this needs the %s role", min))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
