package unpacker

import (
	"fmt"
	"os"

	"github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/types"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

// configFileKeychain resolves credentials from one specific docker config
// file.
//
// go-containerregistry's DefaultKeychain reads whatever DOCKER_CONFIG points
// at, which meant --config had to be honoured by setting that variable
// process-wide. Loading the file directly keeps the resolution — including
// credential helpers, which docker's own config loader applies — without
// touching global state.
type configFileKeychain struct {
	path string
}

func (k configFileKeychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	f, err := os.Open(k.path)
	if err != nil {
		return nil, fmt.Errorf("open docker config %s: %w", k.path, err)
	}
	defer f.Close()

	cf, err := config.LoadFromReader(f)
	if err != nil {
		return nil, fmt.Errorf("parse docker config %s: %w", k.path, err)
	}

	// Same lookup order as the default keychain: the full repository first,
	// then the registry, with docker.io spelled the way the config file does.
	var cfg, empty types.AuthConfig
	for _, key := range []string{target.String(), target.RegistryStr()} {
		if key == name.DefaultRegistry {
			key = authn.DefaultAuthKey
		}
		cfg, err = cf.GetAuthConfig(key)
		if err != nil {
			return nil, fmt.Errorf("read credentials for %s: %w", key, err)
		}
		cfg.ServerAddress = ""
		if cfg != empty {
			break
		}
	}
	if cfg == empty {
		return authn.Anonymous, nil
	}

	return authn.FromConfig(authn.AuthConfig{
		Username:      cfg.Username,
		Password:      cfg.Password,
		Auth:          cfg.Auth,
		IdentityToken: cfg.IdentityToken,
		RegistryToken: cfg.RegistryToken,
	}), nil
}
