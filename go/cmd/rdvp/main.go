package main

import (
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	mrand "math/rand"
	"os"
	"strings"

	"berty.tech/berty/v2/go/internal/ipfsutil"
	"berty.tech/berty/v2/go/internal/logutil"
	"berty.tech/berty/v2/go/pkg/errcode"
	libp2p "github.com/libp2p/go-libp2p"
	libp2p_cicuit "github.com/libp2p/go-libp2p-circuit"
	libp2p_ci "github.com/libp2p/go-libp2p-core/crypto" // nolint:staticcheck
	libp2p_host "github.com/libp2p/go-libp2p-core/host"
	libp2p_peer "github.com/libp2p/go-libp2p-core/peer"
	libp2p_quic "github.com/libp2p/go-libp2p-quic-transport"
	libp2p_rp "github.com/libp2p/go-libp2p-rendezvous"
	libp2p_rpdb "github.com/libp2p/go-libp2p-rendezvous/db/sqlite"
	"github.com/oklog/run"
	ff "github.com/peterbourgon/ff/v3"
	"github.com/peterbourgon/ff/v3/ffcli"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"moul.io/srand"
)

func main() {
	log.SetFlags(0)

	// opts
	var (
		logFormat      = "color"                        // json, console, color, light-console, light-color
		logToFile      = "stderr"                       // can be stdout, stderr or a file path
		logFilters     = "info,warn:bty,bty.* error+:*" // info and warn for bty* + all namespaces for errors, panics, dpanics and fatals
		serveURN       = ":memory:"
		serveListeners = "/ip4/0.0.0.0/tcp/4040,/ip4/0.0.0.0/udp/4141/quic"
		servePK        = ""
		genkeyType     = "Ed25519"
		genkeyLength   = 2048
	)

	// parse opts
	var (
		globalFlags = flag.NewFlagSet("berty", flag.ExitOnError)
		serveFlags  = flag.NewFlagSet("serve", flag.ExitOnError)
		genkeyFlags = flag.NewFlagSet("genkey", flag.ExitOnError)
	)
	globalFlags.StringVar(&logFilters, "logfilters", logFilters, "logged namespaces")
	globalFlags.StringVar(&logToFile, "logfile", logToFile, "if specified, will log everything in JSON into a file and nothing on stderr")
	globalFlags.StringVar(&logFormat, "logformat", logFormat, "if specified, will override default log format")
	serveFlags.StringVar(&serveURN, "db", serveURN, "rdvp sqlite URN")
	serveFlags.StringVar(&serveListeners, "l", serveListeners, "lists of listeners of (m)addrs separate by a comma")
	serveFlags.StringVar(&servePK, "pk", servePK, "private key (generated by `rdvp genkey`)")
	genkeyFlags.StringVar(&genkeyType, "type", genkeyType, "Type of the private key generated, one of : Ed25519, ECDSA, Secp256k1, RSA")
	genkeyFlags.IntVar(&genkeyLength, "length", genkeyLength, "The length (in bits) of the key generated.")

	serve := &ffcli.Command{
		Name:       "serve",
		ShortUsage: "serve -l <maddrs> -pk <private_key> -db <file>",
		FlagSet:    serveFlags,
		Options:    []ff.Option{ff.WithEnvVarPrefix("RDVP")},
		Exec: func(ctx context.Context, args []string) error {
			mrand.Seed(srand.Secure())
			logger, cleanup, err := logutil.NewLogger(logFilters, logFormat, logToFile)
			if err != nil {
				return errcode.TODO.Wrap(err)
			}
			defer cleanup()

			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			laddrs := strings.Split(serveListeners, ",")
			listeners, err := ipfsutil.ParseAddrs(laddrs...)
			if err != nil {
				return errcode.TODO.Wrap(err)
			}

			// load existing or generate new identity
			var priv libp2p_ci.PrivKey
			if servePK != "" {
				kBytes, err := base64.StdEncoding.DecodeString(servePK)
				if err != nil {
					return errcode.TODO.Wrap(err)
				}
				priv, err = libp2p_ci.UnmarshalPrivateKey(kBytes)
				if err != nil {
					return errcode.TODO.Wrap(err)
				}
			} else {
				// Don't use key params here, this is a dev tool, a real installation should use a static key.
				priv, _, err = libp2p_ci.GenerateKeyPairWithReader(libp2p_ci.Ed25519, -1, crand.Reader) // nolint:staticcheck
				if err != nil {
					return errcode.TODO.Wrap(err)
				}
			}

			// init p2p host
			host, err := libp2p.New(ctx,
				// default tpt + quic
				libp2p.DefaultTransports,
				libp2p.Transport(libp2p_quic.NewTransport),

				// Nat & Relay service
				libp2p.EnableNATService(),
				libp2p.DefaultStaticRelays(),
				libp2p.EnableRelay(libp2p_cicuit.OptHop),

				// swarm listeners
				libp2p.ListenAddrs(listeners...),

				// identity
				libp2p.Identity(priv),
			)
			if err != nil {
				return errcode.TODO.Wrap(err)
			}
			defer host.Close()
			logHostInfo(logger, host)

			db, err := libp2p_rpdb.OpenDB(ctx, serveURN)
			if err != nil {
				return errcode.TODO.Wrap(err)
			}

			defer db.Close()

			// start service
			_ = libp2p_rp.NewRendezvousService(host, db)

			<-ctx.Done()
			if err = ctx.Err(); err != nil {
				return errcode.TODO.Wrap(err)
			}
			return nil
		},
	}

	genkey := &ffcli.Command{
		Name:    "genkey",
		FlagSet: genkeyFlags,
		Exec: func(context.Context, []string) error {
			keyType, ok := keyNameToKeyType[strings.ToLower(genkeyType)]
			if !ok {
				return fmt.Errorf("unknown key type : '%s'. Only Ed25519, ECDSA, Secp256k1, RSA supported", genkeyType)
			}
			priv, _, err := libp2p_ci.GenerateKeyPairWithReader(keyType, genkeyLength, crand.Reader) // nolint:staticcheck
			if err != nil {
				return errcode.TODO.Wrap(err)
			}

			kBytes, err := libp2p_ci.MarshalPrivateKey(priv)
			if err != nil {
				return errcode.TODO.Wrap(err)
			}

			fmt.Println(base64.StdEncoding.EncodeToString(kBytes))
			return nil
		},
	}

	root := &ffcli.Command{
		ShortUsage:  "rdvp [global flags] <subcommand> [flags] [args...]",
		FlagSet:     globalFlags,
		Options:     []ff.Option{ff.WithEnvVarPrefix("RDVP")},
		Subcommands: []*ffcli.Command{serve, genkey},
		Exec: func(context.Context, []string) error {
			return flag.ErrHelp
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var process run.Group
	// handle close signal
	execute, interrupt := run.SignalHandler(ctx, os.Interrupt)
	process.Add(execute, interrupt)

	// add root command to process
	process.Add(func() error {
		return root.ParseAndRun(ctx, os.Args[1:])
	}, func(error) {
		cancel()
	})

	// run process
	if err := process.Run(); err != nil && err != context.Canceled {
		log.Fatal(err)
	}
}

// Names are in lower case.
var keyNameToKeyType = map[string]int{
	"ed25519":   libp2p_ci.Ed25519,
	"ecdsa":     libp2p_ci.ECDSA,
	"secp256k1": libp2p_ci.Secp256k1,
	"rsa":       libp2p_ci.RSA,
}

// helpers

func logHostInfo(l *zap.Logger, host libp2p_host.Host) {
	// print peer addrs
	fields := []zapcore.Field{
		zap.String("host ID (local)", host.ID().String()),
	}

	addrs := host.Addrs()
	pi := libp2p_peer.AddrInfo{
		ID:    host.ID(),
		Addrs: addrs,
	}
	if maddrs, err := libp2p_peer.AddrInfoToP2pAddrs(&pi); err == nil {
		for _, maddr := range maddrs {
			fields = append(fields, zap.Stringer("maddr", maddr))
		}
	}

	l.Info("host started", fields...)
}
