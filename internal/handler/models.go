package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"llm-router/internal/pool"

	"github.com/rs/zerolog"
)

// ModelsHandler handles GET /v1/models requests, returning the registered models
// in OpenAI-compatible format.
type ModelsHandler struct {
	pool   *pool.BackendPool
	logger zerolog.Logger
}

// NewModelsHandler creates a new ModelsHandler.
func NewModelsHandler(p *pool.BackendPool, logger zerolog.Logger) *ModelsHandler {
	return &ModelsHandler{
		pool:   p,
		logger: logger.With().Str("component", "models_handler").Logger(),
	}
}

type modelsResponse struct {
	Object string       `json:"object"`
	Data   []modelEntry `json:"data"`
}

type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ServeHTTP returns the list of available models.
func (h *ModelsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	modelNames := h.pool.ModelNames()

	data := make([]modelEntry, 0, len(modelNames))
	for _, name := range modelNames {
		data = append(data, modelEntry{
			ID:      name,
			Object:  "model",
			Created: time.Now().Unix(),
			OwnedBy: "llm-router",
		})
	}

	resp := modelsResponse{
		Object: "list",
		Data:   data,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.Error().Err(err).Msg("failed to encode models response")
	}
}
