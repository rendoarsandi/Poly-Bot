package core

import (
	"encoding/csv"
	"fmt"
	"os"
	"sync"
	"time"
)

type LogEntry struct {
	Timestamp string
	Level     string // INFO, WARN, ERROR, TRADE
	Asset     string
	Event     string
	Details   string
	Equity    string
}

type CSVLogger struct {
	file     *os.File
	writer   *csv.Writer
	mu       sync.Mutex
	entryCh  chan LogEntry
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func NewCSVLogger(filename string) (*CSVLogger, error) {
	file, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}

	writer := csv.NewWriter(file)

	// Write header if file is new
	info, err := file.Stat()
	if err == nil && info.Size() == 0 {
		writer.Write([]string{"Timestamp", "Level", "Asset", "Event", "Details", "Equity"})
		writer.Flush()
	}

	l := &CSVLogger{
		file:    file,
		writer:  writer,
		entryCh: make(chan LogEntry, 1000),
		stopCh:  make(chan struct{}),
	}

	l.wg.Add(1)
	go l.run()

	return l, nil
}

func (l *CSVLogger) run() {
	defer l.wg.Done()
	ticker := time.NewTicker(2 * time.Second) // Faster flush interval
	defer ticker.Stop()

	count := 0
	for {
		select {
		case entry, ok := <-l.entryCh:
			if !ok {
				return
			}
			l.writeEntry(entry)
			count++
			if count >= 10 {
				l.mu.Lock()
				l.writer.Flush()
				l.mu.Unlock()
				count = 0
			}
		case <-ticker.C:
			l.mu.Lock()
			l.writer.Flush()
			l.mu.Unlock()
			count = 0
		case <-l.stopCh:
			// Drain remaining entries
			close(l.entryCh)
			for entry := range l.entryCh {
				l.writeEntry(entry)
			}
			l.mu.Lock()
			l.writer.Flush()
			l.file.Sync()
			l.mu.Unlock()
			return
		}
	}
}

func (l *CSVLogger) writeEntry(e LogEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writer.Write([]string{
		sanitizeForCSV(e.Timestamp),
		sanitizeForCSV(e.Level),
		sanitizeForCSV(e.Asset),
		sanitizeForCSV(e.Event),
		sanitizeForCSV(e.Details),
		sanitizeForCSV(e.Equity),
	})
}

// sanitizeForCSV prevents CSV injection by escaping characters that trigger formulas (=, +, -, @)
func sanitizeForCSV(s string) string {
	if s == "" {
		return ""
	}
	// If it starts with a sensitive character, prepend a single quote
	first := s[0]
	if first == '=' || first == '+' || first == '-' || first == '@' {
		return "'" + s
	}
	return s
}

func (l *CSVLogger) Log(level, asset, event, details string, equity float64) {
	entry := LogEntry{
		Timestamp: time.Now().Format("2006-01-02 15:04:05.000"),
		Level:     level,
		Asset:     asset,
		Event:     event,
		Details:   details,
		Equity:    fmt.Sprintf("%.2f", equity),
	}

	select {
	case l.entryCh <- entry:
	default:
		// Drop if buffer full to avoid blocking
	}
}

func (l *CSVLogger) Close() {
	l.stopOnce.Do(func() {
		close(l.stopCh)
		l.wg.Wait()
		l.file.Close()
	})
}
