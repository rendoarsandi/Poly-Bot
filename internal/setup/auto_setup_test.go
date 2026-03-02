package setup

import (
	"os"
	"strings"
	"testing"
)

func TestUpdateEnvFile(t *testing.T) {
	// Create a temporary file to act as our .env
	tmpfile, err := os.CreateTemp("", ".env.test.*")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpfile.Name()) // clean up

	// Write initial content
	initialContent := []byte("SOME_OTHER_VAR=value\nPOLY_PK=old_pk\n")
	if _, err := tmpfile.Write(initialContent); err != nil {
		t.Fatalf("Failed to write to temp file: %v", err)
	}
	tmpfile.Close()

	// Create a temporary directory
	tempDir, err := os.MkdirTemp("", "setup_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Change working directory to temp dir
	origWd, _ := os.Getwd()
	os.Chdir(tempDir)
	defer os.Chdir(origWd)

	// Create dummy .env in temp dir
	os.WriteFile(".env", initialContent, 0644)

	pk := "0x1234567890abcdef"
	creds := &APICredentials{
		APIKey:     "test-api-key",
		Secret:     "test-secret",
		Passphrase: "test-passphrase",
	}

	err = updateEnvFile(pk, creds)
	if err != nil {
		t.Fatalf("updateEnvFile failed: %v", err)
	}

	// Read updated content
	content, err := os.ReadFile(".env")
	if err != nil {
		t.Fatalf("Failed to read updated .env: %v", err)
	}
	contentStr := string(content)

	// Assertions
	if !strings.Contains(contentStr, "SOME_OTHER_VAR=value") {
		t.Errorf("Expected existing var to be preserved. Got: %s", contentStr)
	}
	if !strings.Contains(contentStr, "POLY_PK=0x1234567890abcdef") {
		t.Errorf("Expected updated PK. Got: %s", contentStr)
	}
	if strings.Contains(contentStr, "old_pk") {
		t.Errorf("Expected old PK to be replaced. Got: %s", contentStr)
	}
	if !strings.Contains(contentStr, "POLY_API_KEY=test-api-key") {
		t.Errorf("Expected new API Key. Got: %s", contentStr)
	}
	if !strings.Contains(contentStr, "POLY_API_SECRET=test-secret") {
		t.Errorf("Expected new API Secret. Got: %s", contentStr)
	}
	if !strings.Contains(contentStr, "POLY_PASSPHRASE=test-passphrase") {
		t.Errorf("Expected new Passphrase. Got: %s", contentStr)
	}
}
