// Package tlscert provides TLS certificate management with support for multiple providers.
package tlscert

import (
	"crypto/tls"
	"crypto/x509"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// ServerTLSConfig returns a *tls.Config configured for server use with the given certificate.
func ServerTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
}

// ClientTLSConfig returns a *tls.Config for client use.
// If insecure is true, certificate verification is skipped (break-glass).
func ClientTLSConfig(insecure bool) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: insecure,
	}
}

// GRPCServerCredentials returns a grpc.ServerOption that configures TLS with the given certificate.
func GRPCServerCredentials(cert tls.Certificate) grpc.ServerOption {
	return grpc.Creds(credentials.NewTLS(ServerTLSConfig(cert)))
}

// GRPCClientCredentials returns a grpc.DialOption that configures TLS for client connections.
// If insecure is true, certificate verification is skipped (break-glass).
func GRPCClientCredentials(insecure bool) grpc.DialOption {
	return grpc.WithTransportCredentials(credentials.NewTLS(ClientTLSConfig(insecure)))
}

// InternalClientTLSConfig returns a *tls.Config for clients that trust only the given CA pool.
// Used for internal gRPC connections where peers present certs signed by a private CA.
func InternalClientTLSConfig(caPool *x509.CertPool) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    caPool,
	}
}

// InternalGRPCClientCredentials returns a grpc.DialOption that trusts only the given CA pool.
func InternalGRPCClientCredentials(caPool *x509.CertPool) grpc.DialOption {
	return grpc.WithTransportCredentials(credentials.NewTLS(InternalClientTLSConfig(caPool)))
}
