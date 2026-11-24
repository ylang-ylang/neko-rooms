package config

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

const (
	NetworkModeDirect = "direct"
	NetworkModeRelay  = "relay"
)

// EffectiveNetworkMode preserves direct networking for existing Go callers that
// construct a zero-value Room rather than loading the CLI configuration.
func (s *Room) EffectiveNetworkMode() string {
	if s.NetworkMode == "" {
		return NetworkModeDirect
	}
	return s.NetworkMode
}

func (s *Room) ValidateNetworking() error {
	switch s.EffectiveNetworkMode() {
	case NetworkModeDirect, NetworkModeRelay:
	default:
		return fmt.Errorf("invalid network.mode: expected direct or relay")
	}

	if s.EffectiveNetworkMode() == NetworkModeRelay || s.RelayICEServersFrontend != "" {
		_, err := s.RelayFrontendICEJSON()
		return err
	}
	return nil
}

// RelayFrontendICEJSON accepts RTCIceServer urls as either a string or an array
// and normalizes them to the array form expected by Neko v3. Errors deliberately
// omit input values: URLs and credentials can contain operator secrets.
func (s *Room) RelayFrontendICEJSON() (string, error) {
	const key = "relay.ice_servers_frontend"
	var servers []struct {
		URLs       json.RawMessage `json:"urls"`
		Username   string          `json:"username,omitempty"`
		Credential string          `json:"credential,omitempty"`
	}
	decoder := json.NewDecoder(strings.NewReader(s.RelayICEServersFrontend))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&servers); err != nil {
		return "", fmt.Errorf("invalid %s: expected a JSON array of RTCIceServer objects", key)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", fmt.Errorf("invalid %s: expected a single JSON array", key)
	}

	type iceServer struct {
		URLs       []string `json:"urls"`
		Username   string   `json:"username,omitempty"`
		Credential string   `json:"credential,omitempty"`
	}
	result := make([]iceServer, 0, len(servers))
	hasTURN, hasTCP := false, false
	for i, server := range servers {
		var urls []string
		if err := json.Unmarshal(server.URLs, &urls); err != nil {
			var single string
			if err := json.Unmarshal(server.URLs, &single); err != nil {
				return "", fmt.Errorf("invalid %s: server %d urls must be a string or an array of strings", key, i+1)
			}
			urls = []string{single}
		}
		if len(urls) == 0 {
			return "", fmt.Errorf("invalid %s: server %d must have at least one URL", key, i+1)
		}
		for _, raw := range urls {
			turn, tcp, err := validateICEURL(raw)
			if err != nil {
				return "", fmt.Errorf("invalid %s: server %d has an invalid ICE URL", key, i+1)
			}
			if turn && (server.Username == "" || server.Credential == "") {
				return "", fmt.Errorf("invalid %s: TURN server %d requires username and credential", key, i+1)
			}
			hasTURN = hasTURN || turn
			hasTCP = hasTCP || tcp
		}
		result = append(result, iceServer{URLs: urls, Username: server.Username, Credential: server.Credential})
	}
	if !hasTURN {
		return "", fmt.Errorf("invalid %s: at least one authenticated turn: or turns: server is required", key)
	}
	if !hasTCP {
		return "", fmt.Errorf("invalid %s: at least one TURN TCP or TLS listener is required", key)
	}
	data, err := json.Marshal(result)
	return string(data), err
}

func validateICEURL(raw string) (turn, tcp bool, err error) {
	invalid := fmt.Errorf("invalid ICE URL")
	u, err := url.Parse(raw)
	if err != nil || strings.IndexFunc(raw, unicode.IsSpace) >= 0 {
		return false, false, invalid
	}
	switch u.Scheme {
	case "turn", "turns":
		turn = true
	case "stun", "stuns":
	default:
		return false, false, invalid
	}
	if u.Opaque == "" || u.Host != "" || u.Path != "" || u.Fragment != "" {
		return false, false, invalid
	}
	host, err := url.Parse("//" + u.Opaque)
	if err != nil || host.Hostname() == "" || host.User != nil || host.Path != "" || strings.ContainsAny(u.Opaque, "/?#@\\") {
		return false, false, invalid
	}
	if port := host.Port(); port != "" {
		if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
			return false, false, invalid
		}
	} else if strings.HasSuffix(u.Opaque, ":") {
		return false, false, invalid
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(query) > 1 {
		return false, false, invalid
	}
	transport := ""
	if len(query) > 0 {
		values, ok := query["transport"]
		if !turn || !ok || len(values) != 1 || (values[0] != "tcp" && values[0] != "udp") {
			return false, false, invalid
		}
		transport = values[0]
	}
	tcp = turn && (transport == "tcp" || (u.Scheme == "turns" && transport == ""))
	return turn, tcp, nil
}
