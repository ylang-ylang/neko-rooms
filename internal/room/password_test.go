package room

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/m1k1o/neko-rooms/internal/config"
	"github.com/m1k1o/neko-rooms/internal/utils"
)

func TestCreateGeneratesStrongDistinctPasswordsWhenBlank(t *testing.T) {
	cfg := networkConfig(config.NetworkModeRelay, false)
	manager, _ := newNetworkManager(t, cfg)
	settings := networkSettings("auto-pass")
	settings.UserPass = ""
	settings.AdminPass = ""

	id, err := manager.Create(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	got, err := manager.GetSettings(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	alnum24 := regexp.MustCompile(`^[0-9a-zA-Z]{24}$`)
	if !alnum24.MatchString(got.UserPass) {
		t.Fatalf("expected 24-char alphanumeric UserPass, got len=%d", len(got.UserPass))
	}
	if !alnum24.MatchString(got.AdminPass) {
		t.Fatalf("expected 24-char alphanumeric AdminPass, got len=%d", len(got.AdminPass))
	}
	if got.UserPass == got.AdminPass {
		t.Fatal("generated UserPass and AdminPass must be distinct")
	}
}

func TestGeneratePasswordUnbiasedAndUnique(t *testing.T) {
	alnum24 := regexp.MustCompile(`^[0-9a-zA-Z]{24}$`)
	seen := make(map[string]struct{}, 32)
	for i := 0; i < 32; i++ {
		pw, err := utils.GeneratePassword(utils.RoomPasswordLength)
		if err != nil {
			t.Fatal(err)
		}
		if !alnum24.MatchString(pw) {
			t.Fatalf("unexpected password format: len=%d", len(pw))
		}
		if _, dup := seen[pw]; dup {
			t.Fatal("duplicate password generated")
		}
		seen[pw] = struct{}{}
	}
}

func TestGeneratePasswordRejectionSamplingAndErrors(t *testing.T) {
	orig := utils.BytesGenerator
	t.Cleanup(func() { utils.BytesGenerator = orig })

	// Bytes >= 248 must be rejected (62 * 4 = 248).
	seq := []byte{248, 249, 250, 255, 0, 1, 61, 62, 123, 247}
	offset := 0
	utils.BytesGenerator = func(b []byte) (int, error) {
		for i := range b {
			b[i] = seq[offset%len(seq)]
			offset++
		}
		return len(b), nil
	}
	pw, err := utils.GeneratePassword(6)
	if err != nil {
		t.Fatal(err)
	}
	if pw != "01Z0ZZ" {
		t.Fatalf("expected unbiased sequence 01Z0ZZ, got %q", pw)
	}

	utils.BytesGenerator = func(b []byte) (int, error) {
		return 0, errors.New("entropy source failed")
	}
	if _, err := utils.GeneratePassword(24); err == nil {
		t.Fatal("expected error when entropy source fails")
	}
	if _, err := utils.GeneratePassword(0); err == nil {
		t.Fatal("expected error for non-positive length")
	}
}
