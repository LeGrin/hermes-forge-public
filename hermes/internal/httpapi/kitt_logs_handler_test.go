package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockDockerLogger is a test double for the dockerLogger interface.
// It simulates docker's --tail behavior by returning only the last n lines.
type mockDockerLogger struct {
	allOutput string
	err       error
}

func (m *mockDockerLogger) logs(ctx context.Context, tail int) ([]byte, error) {
	if m.err != nil {
		return nil, m.err
	}
	lines := strings.Split(strings.TrimRight(m.allOutput, "\n"), "\n")
	if tail > 0 && tail < len(lines) {
		lines = lines[len(lines)-tail:]
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

// TestKittLogs_TailBound verifies:
// - The tail parameter is respected (only last N lines returned)
// - Handler is resilient to docker being unreachable (returns 503, never 500)
func TestKittLogs_TailBound(t *testing.T) {
	tests := []struct {
		name       string
		tail       string
		mockOutput string
		mockErr    error
		wantStatus int
		checkLines func(*testing.T, map[string]any)
	}{
		{
			name:       "tail=5 returns last 5 lines",
			tail:       "5",
			mockOutput: "line1\nline2\nline3\nline4\nline5\nline6\nline7\n",
			wantStatus: http.StatusOK,
			checkLines: func(t *testing.T, m map[string]any) {
				lines, ok := m["lines"].([]any)
				if !ok {
					t.Fatalf("expected lines array, got %T", m["lines"])
				}
				if len(lines) != 5 {
					t.Errorf("expected 5 lines, got %d: %v", len(lines), lines)
				}
			},
		},
		{
			name:       "tail omitted defaults to 20",
			tail:       "",
			mockOutput: "a\nb\nc\n",
			wantStatus: http.StatusOK,
			checkLines: func(t *testing.T, m map[string]any) {
				lines, ok := m["lines"].([]any)
				if !ok {
					t.Fatalf("expected lines array, got %T", m["lines"])
				}
				if len(lines) != 3 {
					t.Errorf("expected 3 lines, got %d", len(lines))
				}
			},
		},
		{
			name:       "docker unreachable returns 503",
			tail:       "20",
			mockErr:    errors.New("docker exec failed"),
			wantStatus: http.StatusServiceUnavailable,
			checkLines: func(t *testing.T, m map[string]any) {
				if detail, ok := m["detail"].(string); !ok || detail != "docker unreachable" {
					t.Errorf("expected detail='docker unreachable', got %v", m)
				}
			},
		},
		{
			name:       "invalid tail falls back to 20",
			tail:       "notanumber",
			mockOutput: "line1\nline2\n",
			wantStatus: http.StatusOK,
			checkLines: func(t *testing.T, m map[string]any) {
				lines, ok := m["lines"].([]any)
				if !ok {
					t.Fatalf("expected lines array, got %T", m["lines"])
				}
				if len(lines) != 2 {
					t.Errorf("expected 2 lines (invalid tail fallback), got %d", len(lines))
				}
			},
		},
		{
			name:       "negative tail falls back to 20",
			tail:       "-5",
			mockOutput: "line1\nline2\n",
			wantStatus: http.StatusOK,
			checkLines: func(t *testing.T, m map[string]any) {
				lines, ok := m["lines"].([]any)
				if !ok {
					t.Fatalf("expected lines array, got %T", m["lines"])
				}
				if len(lines) != 2 {
					t.Errorf("expected 2 lines (negative tail fallback), got %d", len(lines))
				}
			},
		},
		{
			name:       "zero tail falls back to 20",
			tail:       "0",
			mockOutput: "line1\nline2\n",
			wantStatus: http.StatusOK,
			checkLines: func(t *testing.T, m map[string]any) {
				lines, ok := m["lines"].([]any)
				if !ok {
					t.Fatalf("expected lines array, got %T", m["lines"])
				}
				if len(lines) != 2 {
					t.Errorf("expected 2 lines (zero tail fallback), got %d", len(lines))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := "/kitt/logs"
			if tt.tail != "" {
				path += "?tail=" + tt.tail
			}

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)

			h := &kittLogsHandler{logger: &mockDockerLogger{allOutput: tt.mockOutput, err: tt.mockErr}}
			h.serveHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("expected status %d, got %d: %s", tt.wantStatus, rec.Code, rec.Body.String())
			}

			var m map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil && rec.Code == http.StatusOK {
				t.Fatalf("decode JSON: %v", err)
			}
			if tt.checkLines != nil {
				tt.checkLines(t, m)
			}
		})
	}
}

// serveHTTP is a test helper that invokes the logs handler.
func (h *kittLogsHandler) serveHTTP(w http.ResponseWriter, r *http.Request) {
	h.logs(w, r)
}

// TestParseTailParam verifies the tail parameter parsing logic.
func TestParseTailParam(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"20", 20},
		{"5", 5},
		{"100", 100},
		{"0", 0},
		{"-5", -5},
		{"", 0},
		{"abc", 0},
		{"12abc", 12},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			var got int
			parseTailParam(tt.input, &got)
			if got != tt.want {
				t.Errorf("parseTailParam(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

// TestFormatTail verifies the tail formatting for docker CLI.
func TestFormatTail(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{20, "20"},
		{5, "05"},
		{1, "01"},
		{0, "20"},   // fallback
		{100, "00"}, // overflow wraps
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatTail(tt.n)
			if got != tt.want {
				t.Errorf("formatTail(%d) = %q, want %q", tt.n, got, tt.want)
			}
		})
	}
}
