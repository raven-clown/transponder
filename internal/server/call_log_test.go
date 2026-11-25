package server_test

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/server"
)

const promptMarker = "marker-7f3a-prompt"

func TestCallLogLine(t *testing.T) {
	tests := []struct {
		name    string
		on      bool
		prompt  string
		wantLog bool
	}{
		{"off writes no line", false, ping, false},
		{"on writes one call line", true, ping, true},
		{"on never holds the prompt", true, `{"model":"m","messages":[{"role":"user","content":"` + promptMarker + `"}]}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := &logBuffer{}
			e := setupDeps(t, echoRoutes(echo.New("echo")), func(d *server.Deps) {
				d.Logger = slog.New(slog.NewTextHandler(logs, nil))
				d.LogRequests = tt.on
			})
			if _, err := e.do(http.MethodPost, map[string]string{"Authorization": "Bearer " + e.key, "X-Request-ID": "req-42"}, tt.prompt); err != nil {
				t.Fatal(err)
			}
			out := logs.String()
			if strings.Contains(out, promptMarker) {
				t.Fatalf("log holds the prompt: %s", out)
			}
			lines := []string{}
			for _, l := range strings.Split(out, "\n") {
				if strings.Contains(l, "msg=call ") {
					lines = append(lines, l)
				}
			}
			if !tt.wantLog {
				if len(lines) != 0 {
					t.Fatalf("call lines = %d, want 0: %s", len(lines), out)
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("call lines = %d, want 1: %s", len(lines), out)
			}
			for _, want := range []string{
				"request_id=req-42", "endpoint=chat.completions", "provider=echo", "model=m",
				"status=200", "duration=", "cost_nusd=0", "user=alice", "key=alice",
			} {
				if !strings.Contains(lines[0], want) {
					t.Fatalf("call line lacks %q: %s", want, lines[0])
				}
			}
			if tt.prompt == ping && !strings.Contains(lines[0], "input_tokens=1 output_tokens=3") {
				t.Fatalf("call line lacks the token counts: %s", lines[0])
			}
		})
	}
}
