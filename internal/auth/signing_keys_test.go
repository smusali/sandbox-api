package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strconv"
	"testing"

	"github.com/go-jose/go-jose/v4"
)

func TestSigningKeysBounds(t *testing.T) {
	access, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	const accessKID = "access-1"
	enc := publicTestJWK(access, "enc")
	enc.Use = "enc"
	otherAlg := publicTestJWK(access, "rs384")
	otherAlg.Algorithm = "RS384"

	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: &ec.PublicKey, KeyID: "ec", Algorithm: string(jose.ES256), Use: "sig"},
		publicTestJWK(small, "small"),
		enc,
		otherAlg,
		publicTestJWK(access, accessKID),
	}}
	keys, err := signingKeys(set)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[accessKID] == nil {
		t.Fatalf("keys = %d", len(keys))
	}

	set.Keys = nil
	for i := range maxSigningKeys {
		set.Keys = append(set.Keys, publicTestJWK(access, "k"+strconv.Itoa(i)))
	}
	keys, err = signingKeys(set)
	if err != nil || len(keys) != maxSigningKeys {
		t.Fatalf("keys = %d, err = %v", len(keys), err)
	}
	set.Keys = append(set.Keys, publicTestJWK(access, "extra"))
	if _, err := signingKeys(set); !errors.Is(err, errTooManyKeys) {
		t.Fatalf("err = %v", err)
	}

	set.Keys = []jose.JSONWebKey{publicTestJWK(access, "same"), publicTestJWK(access, "same")}
	if _, err := signingKeys(set); !errors.Is(err, errDuplicateKey) {
		t.Fatalf("err = %v", err)
	}
}

func publicTestJWK(priv *rsa.PrivateKey, kid string) jose.JSONWebKey {
	return jose.JSONWebKey{
		Key:       &priv.PublicKey,
		KeyID:     kid,
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}
}
