package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusBadRequest, "invalid_request_error", "missing_field", "", "model is required")
	if rec.Code != http.StatusBadRequest || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, content type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var got map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	e := got["error"]
	if e["message"] != "model is required" || e["type"] != "invalid_request_error" || e["code"] != "missing_field" {
		t.Fatalf("error = %v", e)
	}
	if v, ok := e["param"]; !ok || v != nil {
		t.Fatalf("param = %v (present %v), want null", v, ok)
	}
}

func TestRequestErrorMessage(t *testing.T) {
	if got := (&RequestError{Param: "model", Message: "model is required"}).Error(); got != "model: model is required" {
		t.Fatalf("Error() = %q", got)
	}
	if got := (&RequestError{Message: "bad"}).Error(); got != "bad" {
		t.Fatalf("Error() = %q", got)
	}
}
