package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-playground/validator/v10"
)

// AppError is an error that maps directly to the API error envelope.
type AppError struct {
	Status  int
	Code    string
	Message string
}

func (e *AppError) Error() string { return e.Code + ": " + e.Message }

func NewError(status int, code, message string) *AppError {
	return &AppError{Status: status, Code: code, Message: message}
}

// Fail writes err as the standard envelope; non-AppErrors become 500.
func Fail(w http.ResponseWriter, err error) {
	var ae *AppError
	if errors.As(err, &ae) {
		Error(w, ae.Status, ae.Code, ae.Message)
		return
	}
	slog.Error("unhandled error", "err", err)
	Error(w, http.StatusInternalServerError, "INTERNAL", "internal server error")
}

func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

var validate = validator.New()

// Decode reads a JSON body into dst (unknown fields rejected) and validates struct tags.
func Decode(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return NewError(http.StatusBadRequest, "BAD_REQUEST", "invalid JSON body")
	}
	if err := validate.Struct(dst); err != nil {
		var ve validator.ValidationErrors
		if errors.As(err, &ve) && len(ve) > 0 {
			return NewError(http.StatusBadRequest, "VALIDATION",
				fmt.Sprintf("invalid field: %s", ve[0].Field()))
		}
		return NewError(http.StatusBadRequest, "VALIDATION", "invalid request")
	}
	return nil
}
