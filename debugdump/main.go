// debugdump: a simple debug proxy server for inspecting and forwarding HTTP requests.
// Runs on port 3031 by default. Configure via debugdump_config.json.
//
// Usage: go run ./debugdump
//
// debugdump_config.json example:
//
//	{
//	  "listen": ":3031",
//	  "forward_to": "http://localhost:3000",
//	  "hidden_fields": ["api_key", "password", "secret", "authorization"]
//	}
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type Config struct {
	Listen       string   `json:"listen"`
	ForwardTo    string   `json:"forward_to"`
	HiddenFields []string `json:"hidden_fields"`
}

func loadConfig(path string) Config {
	cfg := Config{
		Listen:    ":3031",
		ForwardTo: "",
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("[debugdump] config file not found (%s), using defaults\n", path)
		return cfg
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("[debugdump] failed to parse config: %v\n", err)
	}
	return cfg
}

// hiddenSet builds a lowercase set for O(1) lookup.
func hiddenSet(fields []string) map[string]struct{} {
	s := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		s[strings.ToLower(f)] = struct{}{}
	}
	return s
}

// filterJSON recursively removes hidden fields from parsed JSON.
func filterJSON(v any, hidden map[string]struct{}) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, child := range val {
			if _, ok := hidden[strings.ToLower(k)]; ok {
				out[k] = "<hidden>"
			} else {
				out[k] = filterJSON(child, hidden)
			}
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = filterJSON(item, hidden)
		}
		return out
	default:
		return v
	}
}

// prettyBody returns a human-readable representation of the body.
// If the body is JSON and hidden fields are configured, those fields are masked.
func prettyBody(body []byte, hidden map[string]struct{}) string {
	if len(body) == 0 {
		return "(empty)"
	}
	if len(hidden) > 0 {
		var parsed any
		if err := json.Unmarshal(body, &parsed); err == nil {
			filtered := filterJSON(parsed, hidden)
			out, err := json.MarshalIndent(filtered, "", "  ")
			if err == nil {
				return string(out)
			}
		}
	}
	// Not JSON or no filtering needed — pretty-print if possible.
	var buf bytes.Buffer
	if err := json.Indent(&buf, body, "", "  "); err == nil {
		return buf.String()
	}
	return string(body)
}

func makeHandler(cfg Config, hidden map[string]struct{}, client *http.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusInternalServerError)
			return
		}
		defer r.Body.Close()

		// --- dump incoming request ---
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n"))
		sb.WriteString(fmt.Sprintf("▶ %s %s %s\n", r.Method, r.RequestURI, r.Proto))
		sb.WriteString(fmt.Sprintf("  Time   : %s\n", start.Format(time.RFC3339)))
		sb.WriteString(fmt.Sprintf("  Remote : %s\n", r.RemoteAddr))
		sb.WriteString("  Headers:\n")
		for k, vs := range r.Header {
			// mask Authorization header value in logs if it's hidden
			if _, ok := hidden[strings.ToLower(k)]; ok {
				sb.WriteString(fmt.Sprintf("    %s: <hidden>\n", k))
			} else {
				sb.WriteString(fmt.Sprintf("    %s: %s\n", k, strings.Join(vs, ", ")))
			}
		}
		sb.WriteString("  Body:\n")
		sb.WriteString(prettyBody(body, hidden))
		sb.WriteString("\n")
		log.Print(sb.String())

		// --- forward if configured ---
		if cfg.ForwardTo == "" {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, `{"message":"debugdump received (no forward_to configured)"}`)
			return
		}

		target := strings.TrimRight(cfg.ForwardTo, "/") + r.RequestURI
		req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
		if err != nil {
			log.Printf("[debugdump] build forward request error: %v\n", err)
			http.Error(w, "failed to build forward request", http.StatusBadGateway)
			return
		}

		// copy all headers from the original request
		for k, vs := range r.Header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[debugdump] forward error: %v\n", err)
			http.Error(w, fmt.Sprintf("forward error: %v", err), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("[debugdump] read response error: %v\n", err)
			http.Error(w, "failed to read upstream response", http.StatusBadGateway)
			return
		}

		// --- dump upstream response ---
		var rs strings.Builder
		rs.WriteString(fmt.Sprintf("◀ %d %s  (upstream %s, %.0fms)\n",
			resp.StatusCode, http.StatusText(resp.StatusCode),
			target, float64(time.Since(start).Milliseconds())))
		rs.WriteString("  Headers:\n")
		for k, vs := range resp.Header {
			rs.WriteString(fmt.Sprintf("    %s: %s\n", k, strings.Join(vs, ", ")))
		}
		rs.WriteString("  Body:\n")
		rs.WriteString(prettyBody(respBody, hidden))
		rs.WriteString("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
		log.Print(rs.String())

		// relay response back to caller
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
	}
}

func main() {
	cfg := loadConfig("debugdump_config.json")
	hidden := hiddenSet(cfg.HiddenFields)

	client := &http.Client{
		Timeout: 5 * time.Minute,
		// Do not follow redirects — pass them back to the caller.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", makeHandler(cfg, hidden, client))

	listen := cfg.Listen
	if listen == "" {
		listen = ":3031"
	}

	if cfg.ForwardTo != "" {
		log.Printf("[debugdump] listening on %s → forwarding to %s\n", listen, cfg.ForwardTo)
	} else {
		log.Printf("[debugdump] listening on %s (dump-only, no forward_to set)\n", listen)
	}
	if len(cfg.HiddenFields) > 0 {
		log.Printf("[debugdump] hiding fields: %s\n", strings.Join(cfg.HiddenFields, ", "))
	}

	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("[debugdump] server error: %v\n", err)
	}
}
