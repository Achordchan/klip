// Command klip-sign makes and uses the release key of the update manifest
// (update.Envelope).
//
//	klip-sign genkey -out key.pem       a new key (PKCS #8 PEM, mode 0600); refuses to overwrite
//	klip-sign pubkey [-key key.pem]     the public key the program is built with (base64)
//	klip-sign sign -in manifest.json -out manifest.signed.json [-key key.pem]
//	klip-sign verify -in manifest.signed.json [-pub base64]
//
// Without -key the key is read from the environment, KLIP_MANIFEST_KEY (the
// PEM itself, as a GitHub secret gives it): it is never an argument. verify
// defaults to the key built into the program.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"

	"klip/internal/update"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "klip-sign:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: klip-sign genkey|pubkey|sign|verify ...")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	keyPath := fs.String("key", "", "the private key (PEM); KLIP_MANIFEST_KEY when empty")
	out := fs.String("out", "", "file to write")
	in := fs.String("in", "", "file to read")
	pub := fs.String("pub", "", "public key (base64); the program's when empty")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "genkey":
		if *out == "" {
			return errors.New("genkey needs -out")
		}
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		der, err := x509.MarshalPKCS8PrivateKey(priv)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err // an existing key is never replaced
		}
		if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	case "pubkey":
		priv, err := loadKey(*keyPath)
		if err != nil {
			return err
		}
		fmt.Println(base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)))
		return nil
	case "sign":
		if *in == "" || *out == "" {
			return errors.New("sign needs -in and -out")
		}
		priv, err := loadKey(*keyPath)
		if err != nil {
			return err
		}
		manifest, err := os.ReadFile(*in)
		if err != nil {
			return err
		}
		env, err := update.SignManifest(priv, manifest)
		if err != nil {
			return err
		}
		// The key of this build of the program must accept it: a release
		// signed with another key would reach nobody.
		k, err := update.ParsePublicKey(firstNonEmpty(*pub, update.ReleaseKey))
		if err != nil {
			return err
		}
		if _, err := update.OpenEnvelope(k, env); err != nil {
			return fmt.Errorf("the program's public key does not accept this key's signature: %w", err)
		}
		return os.WriteFile(*out, append(env, '\n'), 0o644)
	case "verify":
		if *in == "" {
			return errors.New("verify needs -in")
		}
		k, err := update.ParsePublicKey(firstNonEmpty(*pub, update.ReleaseKey))
		if err != nil {
			return err
		}
		data, err := os.ReadFile(*in)
		if err != nil {
			return err
		}
		manifest, err := update.OpenEnvelope(k, data)
		if err != nil {
			return err
		}
		m, err := update.ParseManifest(manifest)
		if err != nil {
			return err
		}
		fmt.Println("signed manifest of", m.Latest.Version)
		return nil
	}
	return errors.New("unknown command " + args[0])
}

func loadKey(path string) (ed25519.PrivateKey, error) {
	var data []byte
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		data = b
	} else if v := os.Getenv("KLIP_MANIFEST_KEY"); v != "" {
		data = []byte(v)
	} else {
		return nil, errors.New("no key: give -key or set KLIP_MANIFEST_KEY")
	}
	return update.ParsePrivateKeyPEM(data)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
