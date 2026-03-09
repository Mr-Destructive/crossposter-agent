package auth_test

import (
	"github.com/mr-destructive/crossposter-agent/auth"
	"testing"
)

func TestEncryptDecrypt(t *testing.T) {
	key := "12345678901234567890123456789012"
	token := "my-secret-token"

	encrypted, err := auth.EncryptToken(token, key)
	if err != nil {
		t.Fatal("EncryptToken failed:", err)
	}

	decrypted, err := auth.DecryptToken(encrypted, key)
	if err != nil {
		t.Fatal("DecryptToken failed:", err)
	}

	if decrypted != token {
		t.Fatalf("expected %s, got %s", token, decrypted)
	}
}
