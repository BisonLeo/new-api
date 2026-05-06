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
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
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

// collapseTools reduces a "tools" array like
//   [{"name":"Agent","description":"...","input_schema":{...}}, ...]
// into a compact {"names":["Agent", ...]} summary for easier debugging.
func collapseTools(arr []any) any {
	names := make([]any, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if name, ok := m["name"]; ok {
			names = append(names, name)
		}
	}
	return map[string]any{"names": names}
}

// firstUserMsgPreviewChars is the rune count exposed from the first user
// message's text. Must cover index 20 (used by the fingerprint algorithm)
// plus a buffer, so anything >= 21 works; 40 leaves headroom.
const firstUserMsgPreviewChars = 40

// extractFirstTextBlock returns the text of the first text-bearing block in
// a message's "content" field. Handles both the string shape and the array
// shape used by the Anthropic messages API.
func extractFirstTextBlock(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		for _, block := range c {
			bm, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := bm["type"].(string); t != "text" {
				continue
			}
			if text, ok := bm["text"].(string); ok {
				return text
			}
		}
	}
	return ""
}

// previewRunes returns up to n leading runes of s, appending an ellipsis if
// truncated. Rune-based so multi-byte UTF-8 input isn't sliced mid-codepoint.
func previewRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// stripSystemReminders removes all <system-reminder>...</system-reminder>
// wrappers and trims surrounding whitespace. Mirrors the client-side regex
// used when computing the cc_version fingerprint input.
func stripSystemReminders(s string) string {
	const openTag = "<system-reminder>"
	const closeTag = "</system-reminder>"
	for {
		start := strings.Index(s, openTag)
		if start < 0 {
			break
		}
		rest := s[start:]
		end := strings.Index(rest, closeTag)
		if end < 0 {
			break
		}
		s = s[:start] + rest[end+len(closeTag):]
	}
	return strings.TrimSpace(s)
}

// extractFirstTrueUserText mirrors the client's extractFirstMessageText:
//   - string content : strip <system-reminder> wrappers.
//   - array  content : prefer the first text block whose body does not start
//     with "<system-reminder>"; otherwise strip wrappers from the newline-
//     joined concatenation of every text block.
//
// This is the exact text the cc_version fingerprint — SHA256(salt + msg[4] +
// msg[7] + msg[20] + version)[:3] — is computed over, so exposing it in the
// dump lets the fingerprint be reproduced offline from the log.
func extractFirstTrueUserText(content any) string {
	switch c := content.(type) {
	case string:
		return stripSystemReminders(c)
	case []any:
		var texts []string
		for _, block := range c {
			bm, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := bm["type"].(string); t != "text" {
				continue
			}
			text, ok := bm["text"].(string)
			if !ok {
				continue
			}
			if !strings.HasPrefix(text, "<system-reminder>") {
				return text
			}
			texts = append(texts, text)
		}
		if len(texts) > 0 {
			return stripSystemReminders(strings.Join(texts, "\n"))
		}
	}
	return ""
}

// collapseMessages reduces a "messages" array like
//   [{"role":"user","content":"..."}, {"role":"assistant","content":"..."}, ...]
// into a compact {"roles":["user","assistant", ...]} summary that preserves
// the original turn order without dumping content. The first user message's
// text is additionally exposed (truncated) under two keys:
//   - "first_user_text_preview"      — the raw first text block (may begin
//                                      with an auto-injected <system-reminder>).
//   - "first_true_user_text_preview" — the "real" user text after
//                                      extractFirstTrueUserText normalizes
//                                      it. This is the text the cc_version
//                                      fingerprint — SHA256(salt + msg[4] +
//                                      msg[7] + msg[20] + version)[:3] — is
//                                      computed over, so it can be reproduced
//                                      offline from the log.
// Both previews have a companion "..._rune_len" for the full (pre-truncation)
// rune count.
func collapseMessages(arr []any) any {
	roles := make([]any, 0, len(arr))
	var firstUserPreview string
	var firstUserLen int
	var firstTrueUserPreview string
	var firstTrueUserLen int
	foundFirstUser := false
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"]
		if role != nil {
			roles = append(roles, role)
		}
		if !foundFirstUser && role == "user" {
			content := m["content"]
			if text := extractFirstTextBlock(content); text != "" {
				firstUserPreview = previewRunes(text, firstUserMsgPreviewChars)
				firstUserLen = len([]rune(text))
				trueText := extractFirstTrueUserText(content)
				firstTrueUserPreview = previewRunes(trueText, firstUserMsgPreviewChars)
				firstTrueUserLen = len([]rune(trueText))
				foundFirstUser = true
			}
		}
	}
	out := map[string]any{"roles": roles}
	if foundFirstUser {
		out["first_user_text_preview"] = firstUserPreview
		out["first_user_text_rune_len"] = firstUserLen
		out["first_true_user_text_preview"] = firstTrueUserPreview
		out["first_true_user_text_rune_len"] = firstTrueUserLen
	}
	return out
}

// filterSystem rewrites the "system" array so each item's "text" either
// exposes the x-anthropic-billing-header portion (when present) or stays
// hidden. Other keys on each item are preserved.
func filterSystem(arr []any) any {
	const marker = "x-anthropic-billing-header:"
	out := make([]any, len(arr))
	for i, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			out[i] = item
			continue
		}
		newItem := make(map[string]any, len(m))
		for mk, mv := range m {
			if strings.ToLower(mk) != "text" {
				newItem[mk] = mv
				continue
			}
			s, ok := mv.(string)
			if !ok {
				newItem[mk] = "<hidden>"
				continue
			}
			if idx := strings.Index(s, marker); idx >= 0 {
				newItem[mk] = s[idx:]
			} else {
				newItem[mk] = "<hidden>"
			}
		}
		out[i] = newItem
	}
	return out
}

// filterJSON recursively removes hidden fields from parsed JSON.
// Special-cases the Anthropic-style "tools" and "system" arrays so the
// debug dump stays readable while still masking sensitive content.
func filterJSON(v any, hidden map[string]struct{}) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, child := range val {
			lowerK := strings.ToLower(k)
			if lowerK == "tools" {
				if arr, ok := child.([]any); ok {
					out[k] = collapseTools(arr)
					continue
				}
			}
			if lowerK == "messages" {
				if arr, ok := child.([]any); ok {
					out[k] = collapseMessages(arr)
					continue
				}
			}
			if lowerK == "system" {
				if arr, ok := child.([]any); ok {
					out[k] = filterSystem(arr)
					continue
				}
			}
			if _, ok := hidden[lowerK]; ok {
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

func maybeGunzipBody(body []byte, headers http.Header) []byte {
	if len(body) == 0 {
		return body
	}
	if !strings.Contains(strings.ToLower(headers.Get("Content-Encoding")), "gzip") {
		return body
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return body
	}
	defer zr.Close()
	inflated, err := io.ReadAll(zr)
	if err != nil {
		return body
	}
	return inflated
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

		loggedRespBody := maybeGunzipBody(respBody, resp.Header)

		// --- dump upstream response ---
		var rs strings.Builder
		rs.WriteString(fmt.Sprintf("◀ %d %s  (upstream %s, %.0fms)\n",
			resp.StatusCode, http.StatusText(resp.StatusCode),
			target, float64(time.Since(start).Milliseconds())))
		rs.WriteString("  Headers:\n")
		for k, vs := range resp.Header {
			rs.WriteString(fmt.Sprintf("    %s: %s\n", k, strings.Join(vs, ", ")))
		}
		if len(loggedRespBody) != len(respBody) {
			rs.WriteString(fmt.Sprintf("  Body-Encoding: gzip (inflated for log, %d -> %d bytes)\n", len(respBody), len(loggedRespBody)))
		}
		rs.WriteString("  Body:\n")
		rs.WriteString(prettyBody(loggedRespBody, hidden))
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
	workDir, err := os.Getwd()
	if err != nil {
		log.Fatalf("[debugdump] resolve working directory error: %v\n", err)
	}
	logPath := filepath.Join(workDir, time.Now().Format("20060102_150405")+".txt")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Fatalf("[debugdump] open log file error: %v\n", err)
	}
	defer logFile.Close()
	log.SetOutput(io.MultiWriter(os.Stdout, logFile))
	log.SetFlags(log.LstdFlags)

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

	log.Printf("[debugdump] writing logs to %s\n", logPath)
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
