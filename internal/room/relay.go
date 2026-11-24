package room

import (
	"fmt"
	"path"
	"strings"
	"unicode"

	dockerImage "github.com/docker/docker/api/types/image"

	"github.com/m1k1o/neko-rooms/internal/types"
)

// These are the native config filenames searched by the pinned Neko/Viper
// runtime. An existing user config must not be silently hidden by our adapter.
const nekoConfigExtensions = "json toml yaml yml properties props prop hcl tfvars dotenv env ini"

// Neko 3.2.0 does not enable Viper AllowEmptyEnv: an empty IP retrieval env falls
// back to the public-IP service default. A native config-file empty string does
// override that default. This file contains no credentials and lives only in the
// container's temporary layer. It must remain readable after supervisord drops
// to the neko user, including when the wrapper starts with a restrictive umask.
const relayStartupScript = `set -eu
for directory in /etc/neko .; do
  for extension in ` + nekoConfigExtensions + `; do
    if [ -f "$directory/neko.$extension" ]; then
      echo 'relay networking refuses an existing native Neko config; remove it or use direct mode' >&2
      exit 1
    fi
  done
done
relay_config=$(mktemp /tmp/neko-rooms-relay.XXXXXX.yaml)
printf '%s\n' 'webrtc:' '  ip_retrieval_url: ""' '  nat1to1: []' > "$relay_config"
chmod 0444 "$relay_config"
export NEKO_CONFIG="$relay_config"
exec "$@"
`

func validatePrivateSettings(settings types.RoomSettings, image *dockerImage.InspectResponse, mode string) error {
	if err := validatePrivateImage(settings, image, mode); err != nil {
		return fmt.Errorf("%s networking %s", mode, err)
	}
	return nil
}

func validatePrivateImage(settings types.RoomSettings, image *dockerImage.InspectResponse, mode string) error {
	if image.Config == nil || len(image.Config.Entrypoint)+len(image.Config.Cmd) == 0 {
		return fmt.Errorf("requires an image with an inspectable startup command")
	}
	if version, ok := image.Config.Labels["net.m1k1o.neko.api-version"]; ok && version != "3" {
		return fmt.Errorf("requires a Neko API version 3 image; the image metadata reports a different version")
	}
	// Native flags outrank both env and the generated config. Refuse managed
	// keys rather than silently losing the relay invariant to image argv. Also
	// recognize ordinary quoted flags in shell-form image commands.
	for _, argv := range [][]string{image.Config.Entrypoint, image.Config.Cmd} {
		for _, argument := range argv {
			for _, token := range strings.FieldsFunc(argument, func(r rune) bool {
				return unicode.IsSpace(r) || strings.ContainsRune(";|&()", r)
			}) {
				token = strings.Trim(token, "\"'")
				flag, _, _ := strings.Cut(token, "=")
				if !strings.HasPrefix(flag, "--") {
					continue
				}
				key := "NEKO_" + strings.ToUpper(strings.ReplaceAll(strings.TrimPrefix(flag, "--"), ".", "_"))
				if types.IsManagedEnvV3(key, true) || (mode == "http" && (key == "NEKO_SESSION_COOKIE_ENABLED" || key == "NEKO_CAPTURE_VIDEO_CODEC" || key == "NEKO_CAPTURE_AUDIO_CODEC")) {
					return fmt.Errorf("cannot use image CLI overrides of manager-owned Neko settings (use direct mode to retain them)")
				}
			}
		}
	}
	for _, entry := range image.Config.Env {
		if name, value, _ := strings.Cut(entry, "="); name == "NEKO_CONFIG" && value != "" {
			return fmt.Errorf("cannot replace an image's custom NEKO_CONFIG (use direct mode to retain it)")
		}
	}
	workingDir := image.Config.WorkingDir
	if workingDir == "" {
		workingDir = "/"
	}
	for _, mount := range settings.Mounts {
		target := path.Clean(mount.ContainerPath)
		for _, directory := range []string{"/etc/neko", workingDir} {
			for _, extension := range strings.Fields(nekoConfigExtensions) {
				file := path.Join(directory, "neko."+extension)
				if target == "/" || target == file || strings.HasPrefix(file, target+"/") {
					return fmt.Errorf("cannot hide a mounted native Neko config (use direct mode to retain it)")
				}
			}
		}
	}
	return nil
}

func privateContainerCommand(image *dockerImage.InspectResponse, mode string) (entrypoint, command []string) {
	entrypoint = []string{"/bin/sh", "-c"}
	// sh -c consumes the next argument as $0. All original image argv follow it
	// unchanged as $@; exec preserves PID 1 and the image's signal semantics.
	command = []string{strings.ReplaceAll(relayStartupScript, "relay", mode), "neko-rooms-" + mode}
	command = append(command, image.Config.Entrypoint...)
	command = append(command, image.Config.Cmd...)
	return entrypoint, command
}

func composeLiteralStrings(values []string) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = strings.ReplaceAll(value, "$", "$$")
	}
	return result
}
