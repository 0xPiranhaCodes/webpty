package auth_test

import (
	"strings"
	"testing"

	"github.com/0xPiranhaCodes/webpty/internal/auth"
)

var fastParams = auth.PasswordParams{Memory: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}

func TestHashPasswordEncodesArgon2idParameters(t *testing.T) {
	encoded, err := auth.HashPassword("correct horse battery", fastParams)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Fatalf("encoded = %q, want argon2id prefix with parameters", encoded)
	}
	if strings.Contains(encoded, "correct horse battery") {
		t.Fatal("encoded hash contains the plaintext password")
	}
}

func TestHashPasswordUsesRandomSalt(t *testing.T) {
	first, err := auth.HashPassword("correct horse battery", fastParams)
	if err != nil {
		t.Fatal(err)
	}
	second, err := auth.HashPassword("correct horse battery", fastParams)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two hashes of the same password are identical; salt is not random")
	}
}

func TestVerifyPassword(t *testing.T) {
	encoded, err := auth.HashPassword("correct horse battery", fastParams)
	if err != nil {
		t.Fatal(err)
	}

	ok, err := auth.VerifyPassword(encoded, "correct horse battery")
	if err != nil || !ok {
		t.Fatalf("VerifyPassword(correct) = %v, %v; want true, nil", ok, err)
	}

	ok, err = auth.VerifyPassword(encoded, "correct horse batterY")
	if err != nil || ok {
		t.Fatalf("VerifyPassword(wrong) = %v, %v; want false, nil", ok, err)
	}
}

func TestVerifyPasswordUsesEncodedParameters(t *testing.T) {
	other := auth.PasswordParams{Memory: 16 * 1024, Iterations: 2, Parallelism: 2, SaltLength: 16, KeyLength: 32}
	encoded, err := auth.HashPassword("correct horse battery", other)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := auth.VerifyPassword(encoded, "correct horse battery")
	if err != nil || !ok {
		t.Fatalf("VerifyPassword = %v, %v; want true, nil", ok, err)
	}
}

func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	for _, encoded := range []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=8192,t=1,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g",
		"$argon2id$v=18$m=8192,t=1,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g",
		"$argon2id$v=19$m=8192,t=0,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g",
		"$argon2id$v=19$m=8192,t=1,p=1$!!!$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g",
	} {
		if ok, err := auth.VerifyPassword(encoded, "anything"); err == nil || ok {
			t.Errorf("VerifyPassword(%q) = %v, %v; want false, error", encoded, ok, err)
		}
	}
}
