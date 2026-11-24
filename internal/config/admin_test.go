package config

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func TestAdminAuthenticationMode(t *testing.T) {
	for _, test := range []struct {
		name    string
		admin   Admin
		mode    string
		invalid bool
	}{
		{"struct compatibility", Admin{Username: "owner", Password: "secret"}, "basic", false},
		{"configured form", Admin{AuthMode: "form", Username: "owner", Password: "secret"}, "form", false},
		{"explicit basic", Admin{AuthMode: "basic", Username: "owner", Password: "secret"}, "basic", false},
		{"no credentials", Admin{AuthMode: "form"}, "none", false},
		{"legacy no credentials", Admin{}, "none", false},
		{"proxy precedence", Admin{AuthMode: "form", ProxyAuth: "http://auth.test/", Username: "ignored"}, "proxy", false},
		{"username only", Admin{AuthMode: "form", Username: "owner"}, "", true},
		{"password only", Admin{AuthMode: "form", Password: "secret"}, "", true},
		{"partial legacy", Admin{Username: "owner"}, "", true},
		{"unknown mode", Admin{AuthMode: "typo"}, "", true},
		{"unknown with proxy", Admin{AuthMode: "typo", ProxyAuth: "http://auth.test/"}, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			mode, err := test.admin.AuthenticationMode()
			if (err != nil) != test.invalid || mode != test.mode {
				t.Fatalf("got mode=%q err=%v, want mode=%q invalid=%v", mode, err, test.mode, test.invalid)
			}
		})
	}
}

func newServerConfig(t *testing.T) (*Server, *cobra.Command) {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.SetEnvPrefix("NEKO_ROOMS")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()
	cfg := &Server{}
	cmd := &cobra.Command{}
	if err := cfg.Init(cmd); err != nil {
		t.Fatal(err)
	}
	return cfg, cmd
}

func TestConfiguredAdminDefaultsToForm(t *testing.T) {
	cfg, _ := newServerConfig(t)
	cfg.Set()
	if cfg.Admin.AuthMode != "form" || cfg.Admin.Username != "" || cfg.Admin.Password != "" {
		t.Fatalf("unconfigured defaults: %+v", cfg.Admin)
	}
	mode, err := cfg.Admin.AuthenticationMode()
	if err != nil || mode != "none" {
		t.Fatalf("unconfigured server should deliberately remain unauthenticated: %q %v", mode, err)
	}
	t.Setenv("NEKO_ROOMS_ADMIN_PASSWORD", "configured-secret")
	cfg.Set()
	if cfg.Admin.Username != "admin" {
		t.Fatal("password-only config lost implicit admin username")
	}
	mode, err = cfg.Admin.AuthenticationMode()
	if err != nil || mode != "form" {
		t.Fatalf("configured credentials did not default to form: %q %v", mode, err)
	}
}

func TestEmptyConfiguredAdminAuthModeStillUsesForm(t *testing.T) {
	cfg, _ := newServerConfig(t)
	viper.Set("admin.auth_mode", "")
	viper.Set("admin.password", "configured-secret")
	cfg.Set()
	if mode, err := cfg.Admin.AuthenticationMode(); err != nil || mode != "form" {
		t.Fatalf("empty configured auth mode must not select legacy Basic: %q %v", mode, err)
	}
}

func TestAdminAuthModeEnvironmentAndFlag(t *testing.T) {
	cfg, cmd := newServerConfig(t)
	t.Setenv("NEKO_ROOMS_ADMIN_USERNAME", "existing-owner")
	t.Setenv("NEKO_ROOMS_ADMIN_PASSWORD", "existing-secret")
	t.Setenv("NEKO_ROOMS_ADMIN_AUTH_MODE", "basic")
	cfg.Set()
	mode, err := cfg.Admin.AuthenticationMode()
	if err != nil || mode != "basic" || cfg.Admin.Username != "existing-owner" || cfg.Admin.Password != "existing-secret" {
		t.Fatalf("existing credentials / env auth mode not honored: %q %v", mode, err)
	}
	if err := cmd.PersistentFlags().Set("admin.auth_mode", "form"); err != nil {
		t.Fatal(err)
	}
	cfg.Set()
	if cfg.Admin.AuthMode != "form" {
		t.Fatal("explicit CLI flag did not take precedence")
	}
}

func TestInvalidConfiguredAdminFailsClearly(t *testing.T) {
	for _, values := range []map[string]string{
		{"admin.username": "owner"},
		{"admin.username": "", "admin.password": "secret"},
		{"admin.auth_mode": "unknown"},
	} {
		t.Run("invalid", func(t *testing.T) {
			cfg, _ := newServerConfig(t)
			for key, value := range values {
				viper.Set(key, value)
			}
			defer func() {
				if recovered := recover(); recovered == nil || !strings.Contains(recovered.(error).Error(), "invalid admin authentication configuration") {
					t.Fatalf("invalid configuration did not fail clearly: %v", recovered)
				}
			}()
			cfg.Set()
		})
	}
}
