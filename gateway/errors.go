package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type errorBody struct {
	Error string `json:"error"`
	Game  string `json:"game,omitempty"`
}

// writeError sends the gateway's uniform JSON error.
func writeError(w http.ResponseWriter, status int, msg, game string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: msg, Game: game})
}

// errUpstreamStatus is raised by ModifyResponse for a backend status the gateway will not relay.
type errUpstreamStatus struct{ code int }

func (e errUpstreamStatus) Error() string { return fmt.Sprintf("upstream returned %d", e.code) }

// errSigRejected means the backend answered 200 with its signature refusal body.
type errSigRejected struct{}

func (errSigRejected) Error() string { return "upstream rejected gateway signature" }
