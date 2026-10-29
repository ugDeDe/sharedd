package control

import (
	"errors"
	"testing"
)

func TestReservePaidAttemptsLimit(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "paid-user", 50)
	if err := s.ReservePaidAttempts(u.ID, 2, 3); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if err := s.ReservePaidAttempts(u.ID, 1, 3); err != nil {
		t.Fatalf("second reserve: %v", err)
	}
	if err := s.ReservePaidAttempts(u.ID, 1, 3); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("limit: want ErrRateLimited, got %v", err)
	}
	if err := s.ReservePaidAttempts(u.ID, 100, 0); err != nil {
		t.Fatalf("disabled limit must be no-op: %v", err)
	}
}
