package unpacker

import (
	"fmt"
	"os"
)

// Credentials holds authentication information for a registry.
type Credentials struct {
	Username   string
	Password   string
	ConfigPath string
	Public     bool
}

// Resolve determines registry credentials from flags and environment variables.
// Resolution order:
//  1. public=true → no credentials
//  2. configPath set → use docker config file
//  3. USERNAME + PASSWORD env vars → basic auth
//  4. none → error
func Resolve(configPath string, public bool) (*Credentials, error) {
	if public {
		return &Credentials{Public: true}, nil
	}
	if configPath != "" {
		return &Credentials{ConfigPath: configPath}, nil
	}
	username := os.Getenv("USERNAME")
	password := os.Getenv("PASSWORD")
	if username != "" && password != "" {
		return &Credentials{Username: username, Password: password}, nil
	}
	return nil, fmt.Errorf("private registry requires --config or USERNAME/PASSWORD environment variables")
}
