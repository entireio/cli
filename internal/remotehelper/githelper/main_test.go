package githelper

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
)

// Set by helperStub; read by TestMain.
const (
	helperServerEnv   = "GITHELPER_TEST_HELPER_SERVER"
	helperProtocolEnv = "GITHELPER_TEST_HELPER_PROTOCOL"
)

// os.Exit here keeps PASS off helper stdout.
func TestMain(m *testing.M) {
	serverURL := os.Getenv(helperServerEnv)
	if serverURL == "" {
		os.Exit(m.Run())
	}
	protocolVersion := 2
	if v := os.Getenv(helperProtocolEnv); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad %s=%q: %v\n", helperProtocolEnv, v, err)
			os.Exit(1)
		}
		protocolVersion = n
	}
	if err := Run(context.Background(), testTransportURL(serverURL), protocolVersion, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "test helper: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}
