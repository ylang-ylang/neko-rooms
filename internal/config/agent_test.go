package config

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func TestAgentPublicURLFromEnvironment(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	t.Setenv("NEKO_ROOMS_AGENT_PUBLIC_URL", "https://cdp.example.test/rooms/")
	viper.SetEnvPrefix("NEKO_ROOMS")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()
	config := &Server{}
	if err := config.Init(&cobra.Command{}); err != nil {
		t.Fatal(err)
	}
	config.Set()
	if config.AgentPublicURL != "https://cdp.example.test/rooms/" {
		t.Fatalf("unexpected Agent public URL %q", config.AgentPublicURL)
	}
}
