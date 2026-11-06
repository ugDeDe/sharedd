package control

import (
	"os"
	"testing"
)

// Под -race bcrypt cost 10 — ~0.4с на хеш; тестам стойкость хеша не нужна.
func TestMain(m *testing.M) {
	UseMinBcryptCostForTests()
	os.Exit(m.Run())
}
