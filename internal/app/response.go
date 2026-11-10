package app

import (
	"net/http"

	"llmrouter/internal/httpx"
)

func writeErr(w http.ResponseWriter, status int, msg string) { httpx.WriteErr(w, status, msg) }

func writeJSON(w http.ResponseWriter, status int, v any) { httpx.WriteJSON(w, status, v) }
