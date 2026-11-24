package config

import (
	"fmt"
	"path"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type Admin struct {
	Static     string
	PathPrefix string
	ProxyAuth  string
	AuthMode   string
	Username   string
	Password   string
}

// AuthenticationMode resolves the effective manager authentication mode. The
// empty struct value retains Basic auth for existing Go callers; configured
// servers default to form auth instead. Proxy authentication takes precedence.
func (a Admin) AuthenticationMode() (string, error) {
	if a.AuthMode != "" && a.AuthMode != "form" && a.AuthMode != "basic" {
		return "", fmt.Errorf("unknown admin.auth_mode %q: expected form or basic", a.AuthMode)
	}
	if a.ProxyAuth != "" {
		return "proxy", nil
	}
	if a.Username == "" && a.Password == "" {
		return "none", nil
	}
	if a.Username == "" || a.Password == "" {
		return "", fmt.Errorf("admin.username and admin.password must both be configured, or both omitted")
	}
	if a.AuthMode == "form" {
		return "form", nil
	}
	return "basic", nil
}

type Server struct {
	Cert    string
	Key     string
	Bind    string
	Proxy   bool
	CORS    bool
	PProf   bool
	Metrics bool

	AgentPublicURL string
	Admin          Admin
}

func (Server) Init(cmd *cobra.Command) error {
	cmd.PersistentFlags().String("bind", "127.0.0.1:8080", "address/port/socket to serve neko_rooms")
	if err := viper.BindPFlag("bind", cmd.PersistentFlags().Lookup("bind")); err != nil {
		return err
	}

	cmd.PersistentFlags().String("cert", "", "path to the SSL cert used to secure the neko_rooms server")
	if err := viper.BindPFlag("cert", cmd.PersistentFlags().Lookup("cert")); err != nil {
		return err
	}

	cmd.PersistentFlags().String("key", "", "path to the SSL key used to secure the neko_rooms server")
	if err := viper.BindPFlag("key", cmd.PersistentFlags().Lookup("key")); err != nil {
		return err
	}

	cmd.PersistentFlags().Bool("proxy", false, "trust reverse proxy headers")
	if err := viper.BindPFlag("proxy", cmd.PersistentFlags().Lookup("proxy")); err != nil {
		return err
	}

	cmd.PersistentFlags().Bool("cors", false, "enable CORS")
	if err := viper.BindPFlag("cors", cmd.PersistentFlags().Lookup("cors")); err != nil {
		return err
	}

	cmd.PersistentFlags().Bool("pprof", false, "enable pprof endpoint available at /debug/pprof")
	if err := viper.BindPFlag("pprof", cmd.PersistentFlags().Lookup("pprof")); err != nil {
		return err
	}

	cmd.PersistentFlags().Bool("metrics", false, "enable metrics endpoint available at /metrics")
	if err := viper.BindPFlag("metrics", cmd.PersistentFlags().Lookup("metrics")); err != nil {
		return err
	}

	cmd.PersistentFlags().String("agent.public_url", "", "public Agent CDP prefix, e.g. https://cdp.example.test/rooms/ (otherwise infer from request headers)")
	if err := viper.BindPFlag("agent.public_url", cmd.PersistentFlags().Lookup("agent.public_url")); err != nil {
		return err
	}

	// Admin

	cmd.PersistentFlags().String("admin.static", "", "path to neko_rooms admin client files to serve")
	if err := viper.BindPFlag("admin.static", cmd.PersistentFlags().Lookup("admin.static")); err != nil {
		return err
	}

	cmd.PersistentFlags().String("admin.path_prefix", "/", "path prefix for admin client and API")
	if err := viper.BindPFlag("admin.path_prefix", cmd.PersistentFlags().Lookup("admin.path_prefix")); err != nil {
		return err
	}

	cmd.PersistentFlags().String("admin.proxy_auth", "", "require auth: proxy authentication URL, only allow if it returns 200")
	if err := viper.BindPFlag("admin.proxy_auth", cmd.PersistentFlags().Lookup("admin.proxy_auth")); err != nil {
		return err
	}

	cmd.PersistentFlags().String("admin.auth_mode", "form", "admin authentication: form (session login) or basic (legacy browser prompt); proxy_auth takes precedence")
	if err := viper.BindPFlag("admin.auth_mode", cmd.PersistentFlags().Lookup("admin.auth_mode")); err != nil {
		return err
	}

	cmd.PersistentFlags().String("admin.username", "admin", "require auth: admin username")
	if err := viper.BindPFlag("admin.username", cmd.PersistentFlags().Lookup("admin.username")); err != nil {
		return err
	}

	cmd.PersistentFlags().String("admin.password", "", "require auth: admin password")
	if err := viper.BindPFlag("admin.password", cmd.PersistentFlags().Lookup("admin.password")); err != nil {
		return err
	}

	return nil
}

func (s *Server) Set() {
	s.Cert = viper.GetString("cert")
	s.Key = viper.GetString("key")
	s.Bind = viper.GetString("bind")
	s.Proxy = viper.GetBool("proxy")
	s.CORS = viper.GetBool("cors")
	s.PProf = viper.GetBool("pprof")
	s.Metrics = viper.GetBool("metrics")
	s.AgentPublicURL = viper.GetString("agent.public_url")

	s.Admin.Static = viper.GetString("admin.static")
	s.Admin.PathPrefix = path.Join("/", path.Clean(viper.GetString("admin.path_prefix")))
	s.Admin.ProxyAuth = viper.GetString("admin.proxy_auth")
	s.Admin.AuthMode = viper.GetString("admin.auth_mode")
	if s.Admin.AuthMode == "" {
		s.Admin.AuthMode = "form"
	}
	s.Admin.Username = viper.GetString("admin.username")
	s.Admin.Password = viper.GetString("admin.password")
	// The implicit username=admin flag default has historically allowed an
	// unconfigured server to run without authentication. Do not mistake it for
	// explicitly configured, incomplete credentials.
	if s.Admin.Password == "" && !viper.IsSet("admin.username") {
		s.Admin.Username = ""
	}
	if _, err := s.Admin.AuthenticationMode(); err != nil {
		panic(fmt.Errorf("invalid admin authentication configuration: %w", err))
	}
}
