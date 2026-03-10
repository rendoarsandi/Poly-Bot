package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type rawAPILogEntry struct {
	Timestamp    string           `json:"timestamp"`
	Source       string           `json:"source"`
	Method       string           `json:"method"`
	Path         string           `json:"path"`
	StatusCode   int              `json:"status_code,omitempty"`
	RequestBody  string           `json:"request_body,omitempty"`
	ResponseBody string           `json:"response_body,omitempty"`
	Error        string           `json:"error,omitempty"`
	Outcome      string           `json:"outcome,omitempty"`
	LatencyMs    map[string]int64 `json:"latency_ms,omitempty"`
}

type rawAPILogger struct {
	mu   sync.Mutex
	file *os.File
	enc  *json.Encoder
}

func newRawAPILogger(path string) (*rawAPILogger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &rawAPILogger{file: file, enc: json.NewEncoder(file)}, nil
}

func (l *rawAPILogger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	_ = l.file.Sync()
	err := l.file.Close()
	l.file = nil
	return err
}

func (l *rawAPILogger) Log(entry rawAPILogEntry) {
	if l == nil {
		return
	}
	entry.Timestamp = time.Now().Format(time.RFC3339Nano)
	entry.RequestBody = sanitizeDebugBody(entry.RequestBody)
	entry.ResponseBody = sanitizeDebugBody(entry.ResponseBody)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return
	}
	_ = l.enc.Encode(entry)
}

func sanitizeDebugBody(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	var decoded any
	if err := json.Unmarshal([]byte(body), &decoded); err == nil {
		redactSensitiveFields(decoded)
		if clean, err := json.Marshal(decoded); err == nil {
			body = string(clean)
		}
	}
	if len(body) > 16000 {
		return body[:16000] + "...<truncated>"
	}
	return body
}

func redactSensitiveFields(v any) {
	switch val := v.(type) {
	case map[string]any:
		for k, child := range val {
			switch strings.ToLower(k) {
			case "signature":
				val[k] = "[redacted-signature]"
			case "owner":
				val[k] = redactToken(val[k])
			default:
				redactSensitiveFields(child)
			}
		}
	case []any:
		for _, child := range val {
			redactSensitiveFields(child)
		}
	}
}

func redactToken(v any) string {
	s, _ := v.(string)
	if len(s) <= 8 {
		return "[redacted]"
	}
	return s[:4] + "..." + s[len(s)-4:]
}
