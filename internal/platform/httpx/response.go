// Package httpx holds the shared JSON response/error envelope used by every module.
package httpx

import (
	"encoding/json"
	"net/http"
)

type Meta struct {
	Page     int `json:"page,omitempty"`
	PageSize int `json:"page_size,omitempty"`
	Total    int `json:"total,omitempty"`
}

type successBody struct {
	Data any   `json:"data"`
	Meta *Meta `json:"meta,omitempty"`
}

type ErrorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type errorEnvelope struct {
	Error ErrorBody `json:"error"`
}

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func OK(w http.ResponseWriter, data any, meta *Meta) {
	JSON(w, http.StatusOK, successBody{Data: data, Meta: meta})
}

func Created(w http.ResponseWriter, data any) {
	JSON(w, http.StatusCreated, successBody{Data: data})
}

func Error(w http.ResponseWriter, status int, code, message string) {
	JSON(w, status, errorEnvelope{Error: ErrorBody{Code: code, Message: message}})
}
