package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/cenkalti/backoff/v4"

	"github.com/pomerium/pomerium/internal/log"
	"github.com/pomerium/pomerium/internal/tcptunnel"
	pb "github.com/pomerium/pomerium/pkg/grpc/cli"
)

func newTunnel(conn *pb.Connection) (*tcptunnel.Tunnel, string, error) {
	listenAddr := "127.0.0.1:0"
	if conn.ListenAddr != nil {
		listenAddr = *conn.ListenAddr
	}

	pxy, err := getProxy(conn)
	if err != nil {
		return nil, "", err
	}

	var tlsCfg *tls.Config
	if pxy.Scheme == "https" {
		tlsCfg, err = getTLSConfig(conn)
		if err != nil {
			return nil, "", fmt.Errorf("tls: %w", err)
		}
	}

	return tcptunnel.New(
		tcptunnel.WithDestinationHost(conn.GetRemoteAddr()),
		tcptunnel.WithProxyHost(pxy.Host),
		tcptunnel.WithTLSConfig(tlsCfg),
	), listenAddr, nil
}

func getProxy(conn *pb.Connection) (*url.URL, error) {
	host, _, err := net.SplitHostPort(conn.GetRemoteAddr())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", conn.GetRemoteAddr(), err)
	}

	if conn.PomeriumUrl == nil {
		return &url.URL{
			Scheme: "https",
			Host:   net.JoinHostPort(host, "443"),
		}, nil
	}

	u, err := url.Parse(conn.GetPomeriumUrl())
	if err != nil {
		return nil, fmt.Errorf("invalid pomerium url: %w", err)
	}
	if u.Host == u.Hostname() {
		if u.Scheme == "https" {
			u.Host = net.JoinHostPort(u.Host, "443")
		} else {
			u.Host = net.JoinHostPort(u.Host, "80")
		}
	}

	return u, nil
}

func getTLSConfig(conn *pb.Connection) (*tls.Config, error) {
	cfg := &tls.Config{
		//nolint: gosec
		InsecureSkipVerify: conn.GetDisableTlsVerification(),
	}
	if len(conn.GetCaCert()) == 0 {
		return cfg, nil
	}

	rootCA, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("get system cert pool: %w", err)
	}
	if ok := rootCA.AppendCertsFromPEM(conn.GetCaCert()); !ok {
		return nil, fmt.Errorf("failed to append provided certificate")
	}
	cfg.RootCAs = rootCA
	return cfg, nil
}

func tunnelAcceptLoop(ctx context.Context, li net.Listener, tun *tcptunnel.Tunnel) {
	defer li.Close()

	bo := backoff.NewExponentialBackOff()
	bo.MaxElapsedTime = 0

	for {
		conn, err := li.Accept()
		if err != nil {
			// canceled, so ignore the error and return
			if ctx.Err() != nil {
				return
			}

			if nerr, ok := err.(net.Error); ok && nerr.Temporary() {
				log.Warn(ctx).Err(err).Msg("failed to accept local connection")
				select {
				case <-time.After(bo.NextBackOff()):
				case <-ctx.Done():
					return
				}
				continue
			}
			return
		}
		bo.Reset()

		go func() {
			defer func() { _ = conn.Close() }()

			err := tun.Run(ctx, conn)
			if err != nil {
				log.Error(ctx).Err(err).Msg("error serving local connection")
			}
		}()
	}
}
