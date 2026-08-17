package unpacker

import (
	"fmt"
	"log"
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
//  3. UNPACKER_USERNAME + UNPACKER_PASSWORD env vars → basic auth
//  4. USERNAME + PASSWORD env vars (deprecated) → basic auth, with a warning
//  5. none → error
//
// The unprefixed names are a poor fit for a credential: Windows sets USERNAME
// for every login session and CI images commonly export both, so unpacker could
// pick up an unrelated value and send it to a registry. They stay supported so
// existing callers keep working, but they warn.
func Resolve(configPath string, public bool) (*Credentials, error) {
	if public {
		return &Credentials{Public: true}, nil
	}
	if configPath != "" {
		return &Credentials{ConfigPath: configPath}, nil
	}
	if username, password := os.Getenv("UNPACKER_USERNAME"), os.Getenv("UNPACKER_PASSWORD"); username != "" && password != "" {
		return &Credentials{Username: username, Password: password}, nil
	}
	if username, password := os.Getenv("USERNAME"), os.Getenv("PASSWORD"); username != "" && password != "" {
		log.Printf("warning: using deprecated USERNAME/PASSWORD environment variables — set UNPACKER_USERNAME/UNPACKER_PASSWORD instead")
		return &Credentials{Username: username, Password: password}, nil
	}
	return nil, fmt.Errorf("private registry requires --config or UNPACKER_USERNAME/UNPACKER_PASSWORD environment variables")
}
