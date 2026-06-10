package httpapi

import (
	"context"
	"net/http"
	"os/exec"
	"strings"
)

// dockerLogger executes docker commands. Injectable for testing.
type dockerLogger interface {
	logs(ctx context.Context, tail int) ([]byte, error)
}

// realDockerLogger shells docker exec in production.
type realDockerLogger struct{}

func (r *realDockerLogger) logs(ctx context.Context, tail int) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", "logs", "--tail", formatTail(tail), "openclaw-openclaw-gateway-1")
	return cmd.CombinedOutput()
}

// kittLogsHandler serves KITT container logs via the Docker CLI.
// CON-003: enables the Constellation dashboard to show last N log lines
// when the operator clicks the KITT anchor node.
type kittLogsHandler struct {
	logger dockerLogger
}

func newKittLogsHandler() *kittLogsHandler {
	return &kittLogsHandler{logger: &realDockerLogger{}}
}

// registerKittLogs mounts the /kitt/logs endpoint.
func (h *kittLogsHandler) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /kitt/logs", h.logs)
}

// logs shells "docker logs --tail={n} openclaw-openclaw-gateway-1" and
// returns the output as {lines: [...]}.
// Returns 503 with {detail: "docker unreachable"} when docker fails
// (e.g. SupplementaryGroups=docker is missing from the hermes service).
func (h *kittLogsHandler) logs(w http.ResponseWriter, r *http.Request) {
	tail := 20
	if t := r.URL.Query().Get("tail"); t != "" {
		var parsed int
		if _, err := parseTailParam(t, &parsed); err != nil || parsed <= 0 {
			tail = 20
		} else {
			tail = parsed
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), maxLogWait)
	defer cancel()

	out, err := h.logger.logs(ctx, tail)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"detail": "docker unreachable",
		})
		return
	}

	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"lines": lines,
	})
}

const maxLogWait = 10 // seconds

func parseTailParam(s string, out *int) (int, error) {
	*out = 0
	neg := false
	for i, c := range s {
		if i == 0 && c == '-' {
			neg = true
			continue
		}
		if c < '0' || c > '9' {
			return 0, nil
		}
		*out = *out*10 + int(c-'0')
	}
	if neg {
		*out = -*out
	}
	return *out, nil
}

func formatTail(n int) string {
	if n <= 0 {
		return "20"
	}
	if n > 99 {
		n = n % 100
	}
	if n < 10 {
		return string('0') + string('0'+byte(n))
	}
	return string('0'+byte(n/10)) + string('0'+byte(n%10))
}
