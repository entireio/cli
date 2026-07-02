package ticket

import (
	"fmt"

	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

// credUser is the fixed username slot for ticket-platform API credentials in
// the token store; the platform is encoded in the service name.
const credUser = "token"

// credService returns the token-store service name for a platform's API
// credential, e.g. "entire-ticket:linear".
func credService(p Platform) string {
	return "entire-ticket:" + string(p)
}

// SaveToken stores the API credential for the given platform in the OS
// credential store (never in settings).
func SaveToken(p Platform, token string) error {
	if err := tokenstore.Set(credService(p), credUser, token); err != nil {
		return fmt.Errorf("store %s credential: %w", p.DisplayName(), err)
	}
	return nil
}

// LoadToken returns the stored API credential for the platform, or an error
// when none is present.
func LoadToken(p Platform) (string, error) {
	tok, err := tokenstore.Get(credService(p), credUser)
	if err != nil {
		return "", fmt.Errorf("read %s credential (run `entire ticket setup`): %w", p.DisplayName(), err)
	}
	return tok, nil
}

// DeleteToken removes the stored API credential for the platform.
func DeleteToken(p Platform) error {
	if err := tokenstore.Delete(credService(p), credUser); err != nil {
		return fmt.Errorf("remove %s credential: %w", p.DisplayName(), err)
	}
	return nil
}
