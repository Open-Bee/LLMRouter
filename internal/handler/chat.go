package handler

import (
	"io"
	"net/http"

	"github.com/rs/zerolog"
	"github.com/tidwall/gjson"
)

// Router is the interface that the chat handler depends on for routing requests.
type Router interface {
	// Route handles the routing of a chat completion request.
	// It reads the raw body, extracts the model, and forwards to a backend.
	Route(w http.ResponseWriter, r *http.Request, model string, body []byte)
}

// ChatHandler handles POST /v1/chat/completions requests.
type ChatHandler struct {
	router Router
	logger zerolog.Logger
}

// NewChatHandler creates a new ChatHandler.
func NewChatHandler(router Router, logger zerolog.Logger) *ChatHandler {
	return &ChatHandler{
		router: router,
		logger: logger.With().Str("component", "chat_handler").Logger(),
	}
}

// ServeHTTP handles the chat completion request.
func (h *ChatHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":{"message":"method not allowed","type":"invalid_request_error"}}`, http.StatusMethodNotAllowed)
		return
	}

	// Read body once (zero-copy forwarding: we keep the raw bytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.logger.Error().Err(err).Msg("failed to read request body")
		http.Error(w, `{"error":{"message":"failed to read request body","type":"invalid_request_error"}}`, http.StatusBadRequest)
		return
	}

	// Extract model field using gjson (no full JSON parse)
	modelResult := gjson.GetBytes(body, "model")
	if !modelResult.Exists() || modelResult.String() == "" {
		http.Error(w, `{"error":{"message":"missing or empty 'model' field","type":"invalid_request_error"}}`, http.StatusBadRequest)
		return
	}

	modelName := modelResult.String()
	h.router.Route(w, r, modelName, body)
}
